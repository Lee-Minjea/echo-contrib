// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: © 2017 LabStack and Echo contributors

/*
Package jaegertracing provides middleware to Opentracing using Jaeger.

Example:
```
package main
import (

	"github.com/labstack/echo-contrib/jaegertracing"
	"github.com/labstack/echo/v4"

)

	func main() {
	    e := echo.New()
	    // Enable tracing middleware
	    c := jaegertracing.New(e, nil)
	    defer c.Close()

	    e.Logger.Fatal(e.Start(":1323"))
	}

```
*/
package jaegertracing

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	"github.com/uber/jaeger-client-go/config"
)

const defaultComponentName = "echo/v4"

type (
	// TraceConfig defines the config for Trace middleware.
	TraceConfig struct {
		// Skipper defines a function to skip middleware.
		Skipper middleware.Skipper

		// OpenTracing Tracer instance which should be got before
		Tracer opentracing.Tracer

		// ComponentName used for describing the tracing component name
		ComponentName string

		// add req body & resp body to tracing tags
		IsBodyDump bool

		// prevent logging long http request bodies
		LimitHTTPBody bool

		// http body limit size (in bytes)
		// NOTE: don't specify values larger than 60000 as jaeger can't handle values in span.LogKV larger than 60000 bytes
		LimitSize int

		// OperationNameFunc composes operation name based on context. Can be used to override default naming
		OperationNameFunc func(c echo.Context) string
	}
)

var (
	// DefaultTraceConfig is the default Trace middleware config.
	DefaultTraceConfig = TraceConfig{
		Skipper:       middleware.DefaultSkipper,
		ComponentName: defaultComponentName,
		IsBodyDump:    false,

		LimitHTTPBody:     true,
		LimitSize:         60_000,
		OperationNameFunc: defaultOperationName,
	}
)

// New creates an Opentracing tracer and attaches it to Echo middleware.
// Returns Closer do be added to caller function as `defer closer.Close()`
func New(e *echo.Echo, skipper middleware.Skipper) io.Closer {
	// Add Opentracing instrumentation
	defcfg := config.Configuration{
		ServiceName: "echo-tracer",
		Sampler: &config.SamplerConfig{
			Type:  "const",
			Param: 1,
		},
		Reporter: &config.ReporterConfig{
			LogSpans:            true,
			BufferFlushInterval: 1 * time.Second,
		},
	}
	cfg, err := defcfg.FromEnv()
	if err != nil {
		panic("Could not parse Jaeger env vars: " + err.Error())
	}
	tracer, closer, err := cfg.NewTracer()
	if err != nil {
		panic("Could not initialize jaeger tracer: " + err.Error())
	}

	opentracing.SetGlobalTracer(tracer)
	e.Use(TraceWithConfig(TraceConfig{
		Tracer:  tracer,
		Skipper: skipper,
	}))
	return closer
}

// Trace returns a Trace middleware.
// Trace middleware traces http requests and reporting errors.
func Trace(tracer opentracing.Tracer) echo.MiddlewareFunc {
	c := DefaultTraceConfig
	c.Tracer = tracer
	c.ComponentName = defaultComponentName
	return TraceWithConfig(c)
}

// TraceWithConfig returns a Trace middleware with config.
// See: `Trace()`.
func TraceWithConfig(config TraceConfig) echo.MiddlewareFunc {
	if config.Tracer == nil {
		panic("echo: trace middleware requires opentracing tracer")
	}
	if config.Skipper == nil {
		config.Skipper = middleware.DefaultSkipper
	}
	if config.ComponentName == "" {
		config.ComponentName = defaultComponentName
	}
	if config.OperationNameFunc == nil {
		config.OperationNameFunc = defaultOperationName
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			req := c.Request()
			opname := config.OperationNameFunc(c)
			realIP := c.RealIP()
			requestID := getRequestID(c) // request-id generated by reverse-proxy

			var sp opentracing.Span
			var err error

			ctx, err := config.Tracer.Extract(
				opentracing.HTTPHeaders,
				opentracing.HTTPHeadersCarrier(req.Header),
			)

			if err != nil {
				sp = config.Tracer.StartSpan(opname)
			} else {
				sp = config.Tracer.StartSpan(opname, ext.RPCServerOption(ctx))
			}
			defer sp.Finish()

			ext.HTTPMethod.Set(sp, req.Method)
			ext.HTTPUrl.Set(sp, req.URL.String())
			ext.Component.Set(sp, config.ComponentName)
			sp.SetTag("client_ip", realIP)
			sp.SetTag("request_id", requestID)

			// Dump request & response body
			var respDumper *responseDumper
			if config.IsBodyDump {
				// request
				reqBody := []byte{}
				if c.Request().Body != nil {
					reqBody, _ = io.ReadAll(c.Request().Body)

					if config.LimitHTTPBody {
						sp.LogKV("http.req.body", limitString(string(reqBody), config.LimitSize))
					} else {
						sp.LogKV("http.req.body", string(reqBody))
					}
				}

				req.Body = io.NopCloser(bytes.NewBuffer(reqBody)) // reset original request body

				// response
				respDumper = newResponseDumper(c.Response())
				c.Response().Writer = respDumper
			}

			// setup request context - add opentracing span
			reqSpan := req.WithContext(opentracing.ContextWithSpan(req.Context(), sp))
			c.SetRequest(reqSpan)
			defer func() {
				// as we have created new http.Request object we need to make sure that temporary files created to hold MultipartForm
				// files are cleaned up. This is done by http.Server at the end of request lifecycle but Server does not
				// have reference to our new Request instance therefore it is our responsibility to fix the mess we caused.
				//
				// This means that when we are on returning path from handler middlewares up in chain from this middleware
				// can not access these temporary files anymore because we deleted them here.
				if reqSpan.MultipartForm != nil {
					reqSpan.MultipartForm.RemoveAll()
				}
			}()

			// call next middleware / controller
			err = next(c)
			if err != nil {
				c.Error(err) // call custom registered error handler
			}

			status := c.Response().Status
			ext.HTTPStatusCode.Set(sp, uint16(status))

			if err != nil {
				logError(sp, err)
			}

			// Dump response body
			if config.IsBodyDump {
				if config.LimitHTTPBody {
					sp.LogKV("http.resp.body", limitString(respDumper.GetResponse(), config.LimitSize))
				} else {
					sp.LogKV("http.resp.body", respDumper.GetResponse())
				}
			}

			return nil // error was already processed with ctx.Error(err)
		}
	}
}

func limitString(str string, size int) string {
	if len(str) > size {
		return str[:size/2] + "\n---- skipped ----\n" + str[len(str)-size/2:]
	}

	return str
}

func logError(span opentracing.Span, err error) {
	var httpError *echo.HTTPError
	if errors.As(err, &httpError) {
		span.LogKV("error.message", httpError.Message)
	} else {
		span.LogKV("error.message", err.Error())
	}
	span.SetTag("error", true)
}

func getRequestID(ctx echo.Context) string {
	requestID := ctx.Request().Header.Get(echo.HeaderXRequestID) // request-id generated by reverse-proxy
	if requestID == "" {
		requestID = generateToken() // missed request-id from proxy, we generate it manually
	}
	return requestID
}

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func defaultOperationName(c echo.Context) string {
	req := c.Request()
	return "HTTP " + req.Method + " URL: " + c.Path()
}

// TraceFunction wraps funtion with opentracing span adding tags for the function name and caller details
func TraceFunction(ctx echo.Context, fn interface{}, params ...interface{}) (result []reflect.Value) {
	// Get function name
	name := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	// Create child span
	parentSpan := opentracing.SpanFromContext(ctx.Request().Context())
	sp := opentracing.StartSpan(
		"Function - "+name,
		opentracing.ChildOf(parentSpan.Context()))
	defer sp.Finish()

	sp.SetTag("function", name)

	// Get caller function name, file and line
	pc := make([]uintptr, 15)
	n := runtime.Callers(2, pc)
	frames := runtime.CallersFrames(pc[:n])
	frame, _ := frames.Next()
	callerDetails := fmt.Sprintf("%s - %s#%d", frame.Function, frame.File, frame.Line)
	sp.SetTag("caller", callerDetails)

	// Check params and call function
	f := reflect.ValueOf(fn)
	if f.Type().NumIn() != len(params) {
		e := fmt.Sprintf("Incorrect number of parameters calling wrapped function %s", name)
		panic(e)
	}
	inputs := make([]reflect.Value, len(params))
	for k, in := range params {
		inputs[k] = reflect.ValueOf(in)
	}
	return f.Call(inputs)
}

// CreateChildSpan creates a new opentracing span adding tags for the span name and caller details.
// User must call defer `sp.Finish()`
func CreateChildSpan(ctx echo.Context, name string) opentracing.Span {
	parentSpan := opentracing.SpanFromContext(ctx.Request().Context())
	sp := opentracing.StartSpan(
		name,
		opentracing.ChildOf(parentSpan.Context()))
	sp.SetTag("name", name)

	// Get caller function name, file and line
	pc := make([]uintptr, 15)
	n := runtime.Callers(2, pc)
	frames := runtime.CallersFrames(pc[:n])
	frame, _ := frames.Next()
	callerDetails := fmt.Sprintf("%s - %s#%d", frame.Function, frame.File, frame.Line)
	sp.SetTag("caller", callerDetails)

	return sp
}

// NewTracedRequest generates a new traced HTTP request with opentracing headers injected into it
func NewTracedRequest(method string, url string, body io.Reader, span opentracing.Span) (*http.Request, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		panic(err.Error())
	}

	ext.SpanKindRPCClient.Set(span)
	ext.HTTPUrl.Set(span, url)
	ext.HTTPMethod.Set(span, method)
	span.Tracer().Inject(span.Context(),
		opentracing.HTTPHeaders,
		opentracing.HTTPHeadersCarrier(req.Header))

	return req, err
}
