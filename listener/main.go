// Package listener provides code for managing incoming http requests.
// The MIT License (MIT)
//
// Copyright (c) 2017 Samit Pal
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.
package listener

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strings"

	"github.com/rs/xid"
	"github.com/samitpal/influxdb-router/backends"
	"github.com/samitpal/influxdb-router/config"
	"github.com/samitpal/influxdb-router/logging"
	"github.com/samitpal/influxdb-router/stats"
)

type messageContext string

const messageContextKey = messageContext("messageBatchId")

var log = logging.For("listener")

// HTTPListenerConfig holds configs for the http daemon
type HTTPListenerConfig struct {
	Addr              string
	HTTPPort          string
	HTTPSPort         string
	IncomingQueue     chan *backends.Payload
	Secure            bool
	SSLCAServerCert   string
	SSLServerCert     string
	SSLServerKey      string
	SSLClientCertAuth bool
	APIKeyHeaderName  string
	APIConfig         config.APIKeyMap
	HealthCheck       chan bool
	Statsd            *stats.Statsd
}

// httpHandlers has all the routes defined.
func httpHandlers(h *http.ServeMux, config *HTTPListenerConfig) *http.ServeMux {
	h.Handle("/write", logHTTPRequest(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { ingest(w, req, config) })))

	h.Handle("/health", logHTTPRequest(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { health(w, config) })))
	return h
}

func logHTTPRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.RemoteAddr)

		if err != nil {
			host = req.RemoteAddr
		}

		method := req.Method
		apiKey := req.Header.Get("Service-API-Key")
		proto := req.Proto
		userAgent := req.UserAgent()
		var request string
		if req.Method == "POST" {
			request = ""
		} else {
			request = req.RequestURI
		}

		// generate unique string for the message batch
		mid := xid.New()
		ctx := context.WithValue(req.Context(), messageContextKey, mid.String())

		log.Printf("Received message-id: %s, remote-host: %s, uri: %s, http-method: %s, api-key: %s, http-proto: %s, user-agent: %s", mid, host, request, method, config.Mask(apiKey, 4), proto, userAgent)
		next.ServeHTTP(w, req.WithContext(ctx))
	})
}

// HTTPListener accepts connections from a telegraf
// client. Upon a successful client API key validation,
// batches of compressed messages are passed to influxdb via http api.
func HTTPListener(config *HTTPListenerConfig) {
	h := http.NewServeMux()
	h = httpHandlers(h, config)

	//Run in https mode
	if config.Secure {
		httpsPort := config.HTTPSPort
		cfg := &tls.Config{}

		if config.SSLCAServerCert != "" {
			caCert, err := ioutil.ReadFile(config.SSLCAServerCert)
			if err != nil {
				log.Fatal(err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)
			cfg.ClientCAs = caCertPool
		}

		// if ssl client cert authentication is enabled.
		if config.SSLClientCertAuth {
			cfg.ClientAuth = tls.RequireAndVerifyClientCert
		}
		srv := &http.Server{
			Addr:      config.Addr + ":" + httpsPort,
			Handler:   h,
			TLSConfig: cfg,
		}
		go func() {
			log.Infof("InfluxDB Router listening on https %s:%s\n", config.Addr, httpsPort)
			err := srv.ListenAndServeTLS(config.SSLServerCert, config.SSLServerKey)
			if err != nil {
				log.Fatalf("ListenAndServeTLS: %s\n", err)
			}
		}()
		//Run in http mode
	} else {
		httpPort := config.HTTPPort
		go func() {
			log.Infof("InfluxDB Router listening on http %s:%s\n", config.Addr, httpPort)
			err := http.ListenAndServe(config.Addr+":"+httpPort, h)
			if err != nil {
				log.Fatalf("ListenAndServe: %s\n", err)
			}
		}()
	}
}

// ingest is a handler that accepts a batch of compressed data points.
// Each batch is then pushed to the IncomingQueue for downstream destination writing.
func ingest(w http.ResponseWriter, req *http.Request, httpConfig *HTTPListenerConfig) {

	// Validate key on every batch.
	// May or may not be a good idea.
	apiKey := req.Header.Get(httpConfig.APIKeyHeaderName)

	var client string
	xff := req.Header.Get("x-forwarded-for")
	if xff != "" {
		client = xff
	} else {
		client = req.RemoteAddr
	}

	// Check if the api key that the request came with is valid.
	_, valid := httpConfig.APIConfig[apiKey]
	if !valid {
		log.Infof("[client %s, api-key: %s] Not a valid api key\n",
			client, apiKey)
		req.Close = true
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Accept only gzip compressed metrics
	if req.Header.Get("Content-Encoding") != "gzip" {
		log.Info("Gzip encoding header is not set. Closing connection")
		req.Close = true
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Get the Context
	var messageID string
	if token := req.Context().Value(messageContextKey); token != nil {
		messageID = token.(string)
	} else {
		messageID = ""
	}
	// counter metric by api key
	go httpConfig.Statsd.SendStatsdCounterMetric(fmt.Sprintf("influx_router.%s.hits", strings.Replace(httpConfig.APIConfig[apiKey].Name, "-", "_", -1)), 1)

	buf, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Errorf("Error reading request body: %v", err)
		return
	}

	// batch (compressed) size counter metric by api key
	go httpConfig.Statsd.SendStatsdCounterMetric(fmt.Sprintf("influx_router.%s.batch-size-bytes", strings.Replace(httpConfig.APIConfig[apiKey].Name, "-", "_", -1)), len(buf))

	p := backends.Payload{MessageID: messageID, Body: buf, APIKey: apiKey}
	select {
	case httpConfig.IncomingQueue <- &p: // Put the batch into the channel unless it is full
		w.WriteHeader(http.StatusNoContent)
		return
	default:
		w.WriteHeader(http.StatusOK)
		log.Infof("[client-ip: %s, api-key: %s] IncomingQueue Queue full. Discarding batch.", client, config.Mask(apiKey, 4))
		return
	}
}

// health is a handler to respond to load balancer health checks.
func health(w http.ResponseWriter, httpConfig *HTTPListenerConfig) {

	select {
	// Fail lb health checks.
	case <-httpConfig.HealthCheck:
		// set the channel back to true so that subsequent health checks fail as well.
		httpConfig.HealthCheck <- true
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, "Service Unavailable")
	default:
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "Ok")
	}

}
