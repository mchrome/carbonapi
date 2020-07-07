package http

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io/ioutil"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ansel1/merry"
	"github.com/go-graphite/carbonapi/carbonapipb"
	"github.com/go-graphite/carbonapi/cmd/carbonapi/config"
	"github.com/go-graphite/carbonapi/date"
	"github.com/go-graphite/carbonapi/expr"
	"github.com/go-graphite/carbonapi/expr/functions/cairo/png"
	"github.com/go-graphite/carbonapi/expr/helper"
	"github.com/go-graphite/carbonapi/expr/types"
	"github.com/go-graphite/carbonapi/pkg/parser"
	utilctx "github.com/go-graphite/carbonapi/util/ctx"
	pb "github.com/go-graphite/protocol/carbonapi_v3_pb"
	"github.com/lomik/zapwriter"
	uuid "github.com/satori/go.uuid"
	"go.uber.org/zap"
)

func cleanupParams(r *http.Request) {
	// make sure the cache key doesn't say noCache, because it will never hit
	r.Form.Del("noCache")

	// jsonp callback names are frequently autogenerated and hurt our cache
	r.Form.Del("jsonp")

	// Strip some cache-busters.  If you don't want to cache, use noCache=1
	r.Form.Del("_salt")
	r.Form.Del("_ts")
	r.Form.Del("_t") // Used by jquery.graphite.js
}

func setError(w http.ResponseWriter, accessLogDetails *carbonapipb.AccessLogDetails, msg string, status int) {
	http.Error(w, http.StatusText(status)+": "+msg, status)
	accessLogDetails.Reason = msg
	accessLogDetails.HTTPCode = int32(status)
}

func getCacheTimeout(logger *zap.Logger, r *http.Request, defaultTimeout int32) int32 {
	if tstr := r.FormValue("cacheTimeout"); tstr != "" {
		t, err := strconv.Atoi(tstr)
		if err != nil {
			logger.Error("failed to parse cacheTimeout",
				zap.String("cache_string", tstr),
				zap.Error(err),
			)
		} else {
			return int32(t)
		}
	}

	return defaultTimeout
}

func renderHandler(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	uid := uuid.NewV4()

	// TODO: Migrate to context.WithTimeout
	// ctx, _ := context.WithTimeout(context.TODO(), config.Config.ZipperTimeout)
	ctx := utilctx.SetUUID(r.Context(), uid.String())
	username, _, _ := r.BasicAuth()
	requestHeaders := utilctx.GetLogHeaders(ctx)

	logger := zapwriter.Logger("render").With(
		zap.String("carbonapi_uuid", uid.String()),
		zap.String("username", username),
		zap.Any("request_headers", requestHeaders),
	)

	srcIP, srcPort := splitRemoteAddr(r.RemoteAddr)

	accessLogger := zapwriter.Logger("access")
	var accessLogDetails = &carbonapipb.AccessLogDetails{
		Handler:        "render",
		Username:       username,
		CarbonapiUUID:  uid.String(),
		URL:            r.URL.RequestURI(),
		PeerIP:         srcIP,
		PeerPort:       srcPort,
		Host:           r.Host,
		Referer:        r.Referer(),
		URI:            r.RequestURI,
		RequestHeaders: requestHeaders,
	}

	logAsError := false
	defer func() {
		deferredAccessLogging(accessLogger, accessLogDetails, t0, logAsError)
	}()

	ApiMetrics.Requests.Add(1)

	err := r.ParseForm()
	if err != nil {
		setError(w, accessLogDetails, err.Error(), http.StatusBadRequest)
		logAsError = true
		return
	}

	targets := r.Form["target"]
	from := r.FormValue("from")
	until := r.FormValue("until")
	template := r.FormValue("template")
	maxDataPoints, _ := strconv.ParseInt(r.FormValue("maxDataPoints"), 10, 64)
	ctx = utilctx.SetMaxDatapoints(ctx, maxDataPoints)
	useCache := !parser.TruthyBool(r.FormValue("noCache"))
	noNullPoints := parser.TruthyBool(r.FormValue("noNullPoints"))
	// status will be checked later after we'll setup everything else
	format, ok, formatRaw := getFormat(r, pngFormat)

	var jsonp string

	if format == jsonFormat {
		// TODO(dgryski): check jsonp only has valid characters
		jsonp = r.FormValue("jsonp")
	}

	timestampFormat := strings.ToLower(r.FormValue("timestampFormat"))
	if timestampFormat == "" {
		timestampFormat = "s"
	}

	timestampMultiplier := int64(1)
	switch timestampFormat {
	case "s":
		timestampMultiplier = 1
	case "ms", "millisecond", "milliseconds":
		timestampMultiplier = 1000
	case "us", "microsecond", "microseconds":
		timestampMultiplier = 1000000
	case "ns", "nanosecond", "nanoseconds":
		timestampMultiplier = 1000000000
	default:
		setError(w, accessLogDetails, "unsupported timestamp format, supported: 's', 'ms', 'us', 'ns'", http.StatusBadRequest)
		logAsError = true
		return
	}

	responseCacheTimeout := getCacheTimeout(logger, r, config.Config.ResponseCacheConfig.DefaultTimeoutSec)
	backendCacheTimeout := getCacheTimeout(logger, r, config.Config.BackendCacheConfig.DefaultTimeoutSec)

	cleanupParams(r)

	responseCacheKey := r.Form.Encode()

	// normalize from and until values
	qtz := r.FormValue("tz")
	from32 := date.DateParamToEpoch(from, qtz, timeNow().Add(-24*time.Hour).Unix(), config.Config.DefaultTimeZone)
	until32 := date.DateParamToEpoch(until, qtz, timeNow().Unix(), config.Config.DefaultTimeZone)

	accessLogDetails.UseCache = useCache
	accessLogDetails.FromRaw = from
	accessLogDetails.From = from32
	accessLogDetails.UntilRaw = until
	accessLogDetails.Until = until32
	accessLogDetails.Tz = qtz
	accessLogDetails.CacheTimeout = responseCacheTimeout
	accessLogDetails.Format = formatRaw
	accessLogDetails.Targets = targets

	if !ok || !format.ValidRenderFormat() {
		setError(w, accessLogDetails, "unsupported format specified: "+formatRaw, http.StatusBadRequest)
		logAsError = true
		return
	}

	if format == protoV3Format {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			accessLogDetails.HTTPCode = http.StatusBadRequest
			accessLogDetails.Reason = "failed to parse message body: " + err.Error()
			http.Error(w, "bad request (failed to parse format): "+err.Error(), http.StatusBadRequest)
			return
		}

		var pv3Request pb.MultiFetchRequest
		err = pv3Request.Unmarshal(body)

		if err != nil {
			accessLogDetails.HTTPCode = http.StatusBadRequest
			accessLogDetails.Reason = "failed to parse message body: " + err.Error()
			http.Error(w, "bad request (failed to parse format): "+err.Error(), http.StatusBadRequest)
			return
		}

		from32 = pv3Request.Metrics[0].StartTime
		until32 = pv3Request.Metrics[0].StopTime
		targets = make([]string, len(pv3Request.Metrics))
		for i, r := range pv3Request.Metrics {
			targets[i] = r.PathExpression
		}
	}

	if useCache {
		tc := time.Now()
		response, err := config.Config.ResponseCache.Get(responseCacheKey)
		td := time.Since(tc).Nanoseconds()
		ApiMetrics.RenderCacheOverheadNS.Add(td)

		accessLogDetails.CarbonzipperResponseSizeBytes = 0
		accessLogDetails.CarbonapiResponseSizeBytes = int64(len(response))

		if err == nil {
			ApiMetrics.RequestCacheHits.Add(1)
			writeResponse(w, http.StatusOK, response, format, jsonp)
			accessLogDetails.FromCache = true
			return
		}
		ApiMetrics.RequestCacheMisses.Add(1)
	}

	if from32 == until32 {
		setError(w, accessLogDetails, "Invalid or empty time range", http.StatusBadRequest)
		logAsError = true
		return
	}

	defer func() {
		if r := recover(); r != nil {
			logger.Error("panic during eval:",
				zap.String("cache_key", responseCacheKey),
				zap.Any("reason", r),
				zap.Stack("stack"),
			)
		}
	}()

	errors := make(map[string]merry.Error)
	backendCacheKey := backendCacheComputeKey(from, until, targets)
	results, err := backendCacheFetchResults(logger, useCache, backendCacheKey, accessLogDetails)

	if err != nil {
		ApiMetrics.BackendCacheMisses.Add(1)

		results = make([]*types.MetricData, 0)
		values := make(map[parser.MetricRequest][]*types.MetricData)

		for _, target := range targets {
			exp, e, err := parser.ParseExpr(target)
			if err != nil || e != "" {
				msg := buildParseErrorString(target, e, err)
				setError(w, accessLogDetails, msg, http.StatusBadRequest)
				logAsError = true
				return
			}

			ApiMetrics.RenderRequests.Add(1)

			result, err := expr.FetchAndEvalExp(ctx, exp, from32, until32, values)
			if err != nil {
				errors[target] = merry.Wrap(err)
			}

			sort.Sort(helper.ByNameNatural(result))

			results = append(results, result...)
		}

		for mFetch := range values {
			expr.SortMetrics(values[mFetch], mFetch)
		}

		if len(errors) == 0 {
			backendCacheStoreResults(logger, backendCacheKey, results, backendCacheTimeout)
		}
	}

	size := 0
	for _, result := range results {
		size += result.Size()
	}

	var body []byte

	returnCode := http.StatusOK
	if len(results) == 0 {
		// Obtain error code from the errors
		// In case we have only "Not Found" errors, result should be 404
		// Otherwise it should be 500
		returnCode = http.StatusNotFound
		errMsgs := make([]string, 0)
		for _, err := range errors {
			if merry.HTTPCode(err) == 404 || merry.Is(err, parser.ErrSeriesDoesNotExist) {
				continue
			}
			errMsgs = append(errMsgs, err.Error())
			if returnCode < 500 {
				returnCode = merry.HTTPCode(err)
			}
		}
		logger.Debug("error response or no response", zap.Strings("error", errMsgs))
		// Allow override status code for 404-not-found replies.
		if returnCode == 404 {
			returnCode = config.Config.NotFoundStatusCode
		}
		if returnCode >= 500 {
			setError(w, accessLogDetails, "error or no response: "+strings.Join(errMsgs, ","), returnCode)
			logAsError = true
			return
		}
	}

	switch format {
	case jsonFormat:
		if maxDataPoints != 0 {
			types.ConsolidateJSON(maxDataPoints, results)
			accessLogDetails.MaxDataPoints = maxDataPoints
		}

		body = types.MarshalJSON(results, timestampMultiplier, noNullPoints)
	case protoV2Format:
		body, err = types.MarshalProtobufV2(results)
		if err != nil {
			setError(w, accessLogDetails, err.Error(), http.StatusInternalServerError)
			logAsError = true
			return
		}
	case protoV3Format:
		body, err = types.MarshalProtobufV3(results)
		if err != nil {
			setError(w, accessLogDetails, err.Error(), http.StatusInternalServerError)
			logAsError = true
			return
		}
	case rawFormat:
		body = types.MarshalRaw(results)
	case csvFormat:
		body = types.MarshalCSV(results)
	case pickleFormat:
		body = types.MarshalPickle(results)
	case pngFormat:
		body = png.MarshalPNGRequest(r, results, template)
	case svgFormat:
		body = png.MarshalSVGRequest(r, results, template)
	}

	accessLogDetails.Metrics = targets
	accessLogDetails.CarbonzipperResponseSizeBytes = int64(size)
	accessLogDetails.CarbonapiResponseSizeBytes = int64(len(body))

	writeResponse(w, returnCode, body, format, jsonp)

	if len(results) != 0 {
		tc := time.Now()
		config.Config.ResponseCache.Set(responseCacheKey, body, responseCacheTimeout)
		td := time.Since(tc).Nanoseconds()
		ApiMetrics.RenderCacheOverheadNS.Add(td)
	}

	gotErrors := len(errors) > 0
	accessLogDetails.HaveNonFatalErrors = gotErrors
}

func backendCacheComputeKey(from, until string, targets []string) string {
	var backendCacheKey bytes.Buffer
	backendCacheKey.WriteString("from:")
	backendCacheKey.WriteString(from)
	backendCacheKey.WriteString(" until:")
	backendCacheKey.WriteString(until)
	backendCacheKey.WriteString(" targets:")
	backendCacheKey.WriteString(strings.Join(targets, ","))
	return backendCacheKey.String()
}

func backendCacheFetchResults(logger *zap.Logger, useCache bool, backendCacheKey string, accessLogDetails *carbonapipb.AccessLogDetails) ([]*types.MetricData, error) {
	if !useCache {
		return nil, errors.New("useCache is false")
	}

	backendCacheResults, err := config.Config.BackendCache.Get(backendCacheKey)

	if err != nil {
		return nil, err
	}

	var results []*types.MetricData
	cacheDecodingBuf := bytes.NewBuffer(backendCacheResults)
	dec := gob.NewDecoder(cacheDecodingBuf)
	err = dec.Decode(&results)

	if err != nil {
		logger.Error("Error decoding cached backend results")
		return nil, err
	}

	accessLogDetails.UsedBackendCache = true
	ApiMetrics.BackendCacheHits.Add(1)

	return results, nil
}

func backendCacheStoreResults(logger *zap.Logger, backendCacheKey string, results []*types.MetricData, backendCacheTimeout int32) {
	var serializedResults bytes.Buffer
	enc := gob.NewEncoder(&serializedResults)
	err := enc.Encode(results)

	if err != nil {
		logger.Error("Error encoding backend results for caching")
		return
	}

	config.Config.BackendCache.Set(backendCacheKey, serializedResults.Bytes(), backendCacheTimeout)
}
