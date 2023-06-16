package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"text/template"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
)

type ctxLogger string

var LOGGER ctxLogger = "logger"

func LogMiddleware(l *zap.SugaredLogger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			newLogger := l.With(
				"host", r.Host,
				"url", r.RequestURI,
				"protocol", r.Proto,
				"remoteAddr", r.RemoteAddr,
				"verb", r.Method,
			)
			if reqID := r.Context().Value(middleware.RequestIDKey); reqID != nil {
				newLogger = newLogger.With("requestId", reqID)
			}

			ctx := context.WithValue(r.Context(), LOGGER, newLogger)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			t1 := time.Now()
			defer func() {
				newLogger.Infow(
					"Handled request",
					"status", ww.Status(),
					"bytesWritten", ww.BytesWritten(),
					"duration", time.Since(t1),
				)
			}()

			// call the next handler in the chain, passing the response writer and
			// the updated request object with the new context value.
			next.ServeHTTP(ww, r.WithContext(ctx))
		})
	}
}

func main() {
	logger, err := zap.NewDevelopment()
	if err != nil {
		log.Fatalf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	undo := zap.ReplaceGlobals(logger)
	defer undo()

	suggar := logger.Sugar()

	broadcaster := NewBroadcaster(RRDist)

	indexHTML, err := os.ReadFile("index.html")
	if err != nil {
		panic(err)
	}
	indexTemplate := template.Must(template.New("").Parse(string(indexHTML)))

	router := chi.NewRouter()
	// A good base middleware stack
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(LogMiddleware(suggar))
	router.Use(middleware.Recoverer)

	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		logger := r.Context().Value(LOGGER).(*zap.SugaredLogger)
		if err := indexTemplate.Execute(w, "ws://"+r.Host+"/websocket"); err != nil {
			logger.Error(err)
		}
	})
	router.Get("/websocket", webSocketHandler(&broadcaster))
	router.Post("/whip", whipHandler(&broadcaster))
	router.Delete("/whip/{peerID}", whipDeleteHandler(&broadcaster))

	suggar.Fatal(http.ListenAndServe(":8080", router))
}
