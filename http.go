package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	jwtgo "github.com/dgrijalva/jwt-go"
	"github.com/go-chi/chi"
)

type ctxKey string

var plugins = make(map[string]Plugin)

type PluginHTTP interface {
	MiddlewareHTTP(Route, RequestHTTP) (MiddlewareHTTP, bool)
}

const JWTTokenCtxKey ctxKey = "_jwt_token_"
const UseProxyCtxKey ctxKey = "_use_proxy_name_"

func _http(config *Config) chan struct{} {

	ro := chi.NewRouter() // routes
	mw := chi.NewRouter() // middleware

	mw.Use(log.HTTPMiddleware)
	for _, route := range config.Routes {

		// setup CORS if needed...
		var corsMidware MiddlewareHTTP
		if route.CORS != nil {
			block := *route.CORS // copy them here...
			corsMidware = func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					corsHandler(&block).ServeHTTP(w, r)
					next.ServeHTTP(w, r)
				})
			}
			log.Printf("[http] OPTIONS %s added ...", route.Path)
			ro.With(corsMidware).MethodFunc("options", route.Path, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		}

		// add http response routes
		for _, req := range route.Request {
			for _, method := range strings.Split(req.Method, "|") {
				method = strings.TrimSpace(method)
				var midware chi.Middlewares

				// add any method middleware

				// check for JWT authorization
				if req.JWT != nil {
					midware = append(midware, func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

							// req.JWT need to have the sigCtxKey already in the context
							token, err := decodeJWT(w, r, req.JWT)
							if err != nil {
								http.Error(w, err.Error(), 401)
								return
							}

							// go through the claims and see if the strings match
							if mc, ok := token.Claims.(jwtgo.MapClaims); ok {
								for k, v := range mc {
									if reqv, ok := req.JWT.KeyVals[k]; ok {
										if v1, ok := v.(string); ok {
											v2, _ := reqv.Expr.Value(nil)
											if v1 != v2.AsString() {
												ro.NotFoundHandler().ServeHTTP(w, r)
												return
											}
										}
									}
								}
							}

							ctx := r.Context()
							ctx = context.WithValue(ctx, JWTTokenCtxKey, token)
							next.ServeHTTP(w, r.WithContext(ctx))
						})
					})
				}

				// check for POST values
				if strings.ToUpper(method) == http.MethodPost {
					midware = append(midware, func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							for k, v := range req.Posted {
								if v == "*" {
									continue
								}
								if v != r.PostFormValue(k) {
									ro.NotFoundHandler().ServeHTTP(w, r)
									return
								}
							}
							next.ServeHTTP(w, r)
						})
					})
				}

				// add any plugin middleware
				for k, plugin := range plugins {
					if _, ok := plugin.(PluginHTTP); ok {
						if hdlr, ok := plugin.(PluginHTTP).MiddlewareHTTP(route, req); ok {
							log.Printf("[http][%s] %s middleware added ...", k, route.Path)
							midware = append(midware, hdlr)
						}
					}
				}

				// add cors middleware if this handler requests it
				if corsMidware != nil {
					log.Printf("[http] CORS %s added ...", route.Path)
					midware = append(midware, corsMidware)
				}

				if route.Proxy != nil {
					pxy := route.Proxy // capture for the closure...
					midware = append(midware, func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if proxy, ok := r.Context().Value(ctxKey(pxy.Name)).(*configProxy); ok {
								useProxy(proxy, w, r, pxy.Headers) // async call
								return
							}
						})
					})
				}

				// add the handler with the proper middleware
				log.Printf("[http] %s %s added ...", strings.ToUpper(method), route.Path)
				ro.With(midware...).Method(method, route.Path, httpHandler(req))
			}
		}
	}

	// check for custom not found handler
	if config.NotFound != nil {
		ro.NotFound(func(w http.ResponseWriter, r *http.Request) {
			var status = config.NotFound.Response.Status
			n, err := strconv.ParseInt(status, 10, 16)
			log.OnErr(err).Println("[error] not found parse int: %v", err)

			w.WriteHeader(int(n))
			body, _ := config.NotFound.Response.Body.Expr.Value(&bodyEvalCtx)
			fmt.Fprintln(w, body.AsString())
		})
	}

	// check for custom method not allowed handler
	if config.MethodNotAllowed != nil {
		ro.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			var status = config.MethodNotAllowed.Response.Status
			n, err := strconv.ParseInt(status, 10, 16)
			log.OnErr(err).Println("[error] method not allowed parse int: %v", err)

			w.WriteHeader(int(n))
			body, _ := config.MethodNotAllowed.Response.Body.Expr.Value(&bodyEvalCtx)
			fmt.Fprintln(w, body.AsString())
		})
	}

	re := reloadError{os: config.internal.os} // setup error handling on reload

	// check to see if we should send back headers
	// saying that the reload failed
	if !config.internal.svrCfgLoadValid {
		mw.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				scheme := "http://"
				if r.TLS != nil {
					scheme = "https://"
				}
				re.headers(config, w.Header().Add, scheme+r.Host)
				next.ServeHTTP(w, r)
			})
		})
	}

	// show errors and stats
	ro.Get("/_internal/reload/errors", re.handler(config))
	ro.Get("/_internal/server/stats", serverStats())

	// channels used for stopping all of the running servers
	var stoppers = make([]chan struct{}, len(config.Servers))
	for i := range stoppers {
		stoppers[i] = make(chan struct{}, 0)
	}

	// how we can wait until all of the servers have gracefully shutdown
	var svr = new(sync.WaitGroup)
	svr.Add(len(config.Servers))

	for i, server := range config.Servers {
		r := chi.NewRouter() // a place where we can combine middleware and routes

		tlsConfig := useTLS(r, server) // Getting our TLS status for each server
		useJWT(r, server)

		// check if we should limit this server to only HTTP2 requests
		if server.HTTP2 {
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if _, ok := w.(http.Pusher); !ok {
						http.Error(w, http.StatusText(http.StatusUpgradeRequired), http.StatusUpgradeRequired)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
		}

		if server.Proxy != nil {
			log.Printf("[proxy] %q add proxy %q lookup ...", server.Name, server.Proxy.Name)
			urlParsed, err := url.Parse(server.Proxy.URL)
			if err != nil {
				log.Fatalf("[server] %q parse proxy block: %v", server.Proxy.Name, err)
			}
			server.Proxy._url = urlParsed
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := context.WithValue(r.Context(), ctxKey(server.Proxy.Name), server.Proxy)
					next.ServeHTTP(w, r.WithContext(ctx))
				})
			})
		}

		r.Use(mw.Middlewares()...)
		r.Mount("/", ro)
		serve := &http.Server{
			Addr:      server.Host,
			Handler:   r,
			TLSConfig: tlsConfig,
		}

		// handle graceful shutdown for all started servers
		go func() {
			<-stoppers[i]
			defer svr.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := serve.Shutdown(ctx)
			log.OnErr(err).Printf("[server] graceful shutdown err: %v", err)

		}()

		// starting the server
		go func(name string) {
			if tlsConfig == nil {
				log.Printf("[server] %q starting HTTP (addr: %s) ...", name, serve.Addr)
				if err := serve.ListenAndServe(); err != http.ErrServerClosed {
					log.Fatalf("[server] HTTP ListenAndServe: %v", err)
				}
			} else {
				log.Printf("[server] %q starting HTTPS (addr: %s) ...", name, serve.Addr)
				if err := serve.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
					log.Fatalf("[server] HTTPS ListenAndServe: %v", err)
				}
			}
		}(server.Name)
	}

	shutdown := make(chan struct{}, 1)
	go func() {
		<-config.shutdown
		for _, ch := range stoppers {
			close(ch)
		}
		svr.Wait()
		close(shutdown)
	}()
	return shutdown
}

func serverStats() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "addr:", r.Host)
	}
}
