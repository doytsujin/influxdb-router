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
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jamiealquiza/envy"

	"github.com/samitpal/influxdb-router/api"
	"github.com/samitpal/influxdb-router/backends"
	"github.com/samitpal/influxdb-router/config"
	"github.com/samitpal/influxdb-router/listener"
	"github.com/samitpal/influxdb-router/logging"
	"github.com/samitpal/influxdb-router/stats"
	"github.com/samitpal/influxdb-router/writer"
)

var (
	options struct {
		apiAddr            string
		apiPort            string
		authEnabled        bool
		authMode           string
		addr               string
		httpPort           string
		httpsPort          string
		incomingQueuecap   int
		secure             bool
		sslServerCert      string
		sslCAServerCert    string
		sslServerKey       string
		sslClientCertAuth  bool
		configFile         string
		apiKeyHeaderName   string
		waitBeforeShutdown int
		statsdServer       string
		statsInterval      int
		version            bool
	}

	sigChan = make(chan os.Signal)
	log     = logging.For("main")
	version string
	date    string
	commit  string
)

func init() {
	flag.BoolVar(&options.authEnabled, "auth-enabled", false, "Whether to enable authentication when communicating with InfluxDB")
	flag.StringVar(&options.authMode, "auth-mode", "from-config", "Can be either 'from-config or 'from-env' presently. 'auth-enabled' flag needs to be turned on.")
	flag.StringVar(&options.addr, "listen-addr", "0.0.0.0", "InfluxDB router listen address")
	flag.StringVar(&options.httpPort, "listen-http-port", "8090", "InfluxDB router listen port (http)")
	flag.StringVar(&options.apiAddr, "api-listen-addr", "127.0.0.1", "InfluxDB router api listen address")
	flag.StringVar(&options.apiPort, "api-listen-http-port", "8080", "InfluxDB router api listen port")
	flag.StringVar(&options.httpsPort, "listen-https-port", "8443", "InfluxDB router listen port (https)")
	flag.IntVar(&options.incomingQueuecap, "incoming-queue-cap", 500000, "In-flight incoming message queue capacity")
	flag.BoolVar(&options.secure, "secure", false, "Whether to turn on ssl.")
	flag.StringVar(&options.sslCAServerCert, "ssl-ca-server-cert", "", "CA Server TLS Certificate. Useful when client cert based auth is enabled.")
	flag.StringVar(&options.sslServerCert, "ssl-server-cert", "./server.crt", "Server TLS Certificate")
	flag.StringVar(&options.sslServerKey, "ssl-server-key", "./server.key", "Server TLS Key")
	flag.BoolVar(&options.sslClientCertAuth, "ssl-client-cert-auth", false, "Whether to turn on ssl client certificate based auth")
	flag.StringVar(&options.configFile, "config_file", "./config.toml", "Configuration options.")
	flag.StringVar(&options.apiKeyHeaderName, "api-key-header-name", "Service-API-Key", "Name of the API key header.")
	flag.IntVar(&options.waitBeforeShutdown, "wait-before-shutdown", 1, "Number of seconds to wait before the process shuts down. Health checks will be failed during this time.")
	flag.StringVar(&options.statsdServer, "statsd-server", "localhost:8125", "statsd server:port for sending metrics")
	flag.IntVar(&options.statsInterval, "stats-interval", 30, "Interval in seconds for sending statsd metrics.")
	flag.BoolVar(&options.version, "version", false, "version of the binary.")

	envy.Parse("INFLUX")
	flag.Parse()
}

// Handles signal events.
func handleSignals(h chan bool) {
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	// Fail lb health checks.
	h <- true
	log.Infof("Waiting for %d secs before shutdown", options.waitBeforeShutdown)
	timeOut := time.NewTimer(time.Second * time.Duration(options.waitBeforeShutdown))
	<-timeOut.C
	log.Info("Shutting down")
	os.Exit(0)
}

func displayVersion() {
	fmt.Println("Version: ", version)
	fmt.Println("Build Time: ", date)
	fmt.Println("Commit id: ", commit)
	os.Exit(0)
}

func main() {

	if options.version {
		displayVersion()
	}

	log.Info(`
    ____     _____           ___  ___     ___            __
   /  _/__  / _/ /_ ____ __ / _ \/ _ )   / _ \___  __ __/ /____ ____
  _/ // _ \/ _/ / // /\ \ // // / _  |  / , _/ _ \/ // / __/ -_) __/
 /___/_//_/_//_/\_,_//_\_\/____/____/  /_/|_|\___/\_,_/\__/\__/_/
`)

	ready := make(chan bool, 1)

	// Used to fail lb healthchecks.
	healthCheck := make(chan bool, 1)

	incomingQueue := make(chan *backends.Payload, options.incomingQueuecap)

	conf, err := config.NewConfigs(options.configFile)
	if err != nil {
		log.Fatal(err)
	}

	log.Print(conf.LogConfig())

	// Build the ApiKeyMap config now.
	apiConf, err := config.NewAPIKeyMap(conf.Customers, options.authEnabled, options.authMode)
	if err != nil {
		log.Fatal(err)
	}

	// Output writer.
	go writer.OutQueueWriter(apiConf, incomingQueue, ready)

	// start statsd metrics tracker
	c, err := stats.ConnectStatsd(options.statsdServer, "udp")
	if err != nil {
		log.Errorf("Error connecting to statsd server: %v", err)
	}

	sc := stats.Statsd{
		Interval: options.statsInterval,
		Conn:     c,
	}
	go stats.ExportMetrics(&sc, options.incomingQueuecap, incomingQueue, apiConf)

	// wait till the writer is ready.
	<-ready

	// HTTP Listener.
	go listener.HTTPListener(&listener.HTTPListenerConfig{
		Addr:              options.addr,
		HTTPPort:          options.httpPort,
		HTTPSPort:         options.httpsPort,
		IncomingQueue:     incomingQueue,
		Secure:            options.secure,
		SSLCAServerCert:   options.sslCAServerCert,
		SSLServerCert:     options.sslServerCert,
		SSLServerKey:      options.sslServerKey,
		SSLClientCertAuth: options.sslClientCertAuth,
		APIConfig:         apiConf,
		APIKeyHeaderName:  options.apiKeyHeaderName,
		HealthCheck:       healthCheck,
		Statsd:            &sc,
	})

	// API listener.
	go api.HTTPListener(&api.HTTPListenerConfig{
		Addr:     options.apiAddr,
		Port:     options.apiPort,
		TomlConf: *conf,
		APIConf:  apiConf,
	})

	handleSignals(healthCheck)
}
