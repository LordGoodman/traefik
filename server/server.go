package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/armon/go-proxyproto"
	"github.com/containous/mux"
	"github.com/containous/traefik/cluster"
	"github.com/containous/traefik/configuration"
	"github.com/containous/traefik/healthcheck"
	"github.com/containous/traefik/log"
	"github.com/containous/traefik/metrics"
	"github.com/containous/traefik/middlewares"
	"github.com/containous/traefik/middlewares/accesslog"
	mauth "github.com/containous/traefik/middlewares/auth"
	"github.com/containous/traefik/provider"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/server/cookie"
	traefikTls "github.com/containous/traefik/tls"
	"github.com/containous/traefik/types"
	"github.com/containous/traefik/whitelist"
	"github.com/eapache/channels"
	thoas_stats "github.com/thoas/stats"
	"github.com/urfave/negroni"
	"github.com/vulcand/oxy/cbreaker"
	"github.com/vulcand/oxy/connlimit"
	"github.com/vulcand/oxy/forward"
	"github.com/vulcand/oxy/ratelimit"
	"github.com/vulcand/oxy/roundrobin"
	"github.com/vulcand/oxy/utils"
	"golang.org/x/net/http2"
)

var (
	oxyLogger = &OxyLogger{}
)

// Server is the reverse-proxy/load-balancer engine
type Server struct {
	serverEntryPoints             serverEntryPoints
	configurationChan             chan types.ConfigMessage
	configurationValidatedChan    chan types.ConfigMessage
	signals                       chan os.Signal
	stopChan                      chan bool
	providers                     []provider.Provider
	currentConfigurations         safe.Safe
	globalConfiguration           configuration.GlobalConfiguration
	accessLoggerMiddleware        *accesslog.LogHandler
	routinesPool                  *safe.Pool
	leadership                    *cluster.Leadership
	defaultForwardingRoundTripper http.RoundTripper
	metricsRegistry               metrics.Registry
}

type serverEntryPoints map[string]*serverEntryPoint

type serverEntryPoint struct {
	httpServer *http.Server
	listener   net.Listener
	httpRouter *middlewares.HandlerSwitcher
	certs      safe.Safe
}

type serverRoute struct {
	route              *mux.Route
	stripPrefixes      []string
	stripPrefixesRegex []string
	addPrefix          string
	replacePath        string
	replacePathRegex   string
}

// NewServer returns an initialized Server.
func NewServer(globalConfiguration configuration.GlobalConfiguration) *Server {
	server := new(Server)

	server.serverEntryPoints = make(map[string]*serverEntryPoint)
	server.configurationChan = make(chan types.ConfigMessage, 100)
	server.configurationValidatedChan = make(chan types.ConfigMessage, 100)
	server.signals = make(chan os.Signal, 1)
	server.stopChan = make(chan bool, 1)
	server.providers = []provider.Provider{}
	server.configureSignals()
	currentConfigurations := make(types.Configurations)
	server.currentConfigurations.Set(currentConfigurations)
	server.globalConfiguration = globalConfiguration
	if server.globalConfiguration.API != nil {
		server.globalConfiguration.API.CurrentConfigurations = &server.currentConfigurations
	}

	server.routinesPool = safe.NewPool(context.Background())
	server.defaultForwardingRoundTripper = createHTTPTransport(globalConfiguration)

	server.metricsRegistry = metrics.NewVoidRegistry()
	if globalConfiguration.Metrics != nil {
		server.registerMetricClients(globalConfiguration.Metrics)
	}

	if globalConfiguration.Cluster != nil {
		// leadership creation if cluster mode
		server.leadership = cluster.NewLeadership(server.routinesPool.Ctx(), globalConfiguration.Cluster)
	}

	if globalConfiguration.AccessLogsFile != "" {
		globalConfiguration.AccessLog = &types.AccessLog{FilePath: globalConfiguration.AccessLogsFile, Format: accesslog.CommonFormat}
	}

	if globalConfiguration.AccessLog != nil {
		var err error
		server.accessLoggerMiddleware, err = accesslog.NewLogHandler(globalConfiguration.AccessLog)
		if err != nil {
			log.Warnf("Unable to create log handler: %s", err)
		}
	}
	return server
}

// createHTTPTransport creates an http.Transport configured with the GlobalConfiguration settings.
// For the settings that can't be configured in Traefik it uses the default http.Transport settings.
// An exception to this is the MaxIdleConns setting as we only provide the option MaxIdleConnsPerHost
// in Traefik at this point in time. Setting this value to the default of 100 could lead to confusing
// behaviour and backwards compatibility issues.
func createHTTPTransport(globalConfiguration configuration.GlobalConfiguration) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   configuration.DefaultDialTimeout,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}
	if globalConfiguration.ForwardingTimeouts != nil {
		dialer.Timeout = time.Duration(globalConfiguration.ForwardingTimeouts.DialTimeout)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   globalConfiguration.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if globalConfiguration.ForwardingTimeouts != nil {
		transport.ResponseHeaderTimeout = time.Duration(globalConfiguration.ForwardingTimeouts.ResponseHeaderTimeout)
	}
	if globalConfiguration.InsecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	if len(globalConfiguration.RootCAs) > 0 {
		transport.TLSClientConfig = &tls.Config{
			RootCAs: createRootCACertPool(globalConfiguration.RootCAs),
		}
	}
	http2.ConfigureTransport(transport)

	return transport
}

func createRootCACertPool(rootCAs traefikTls.RootCAs) *x509.CertPool {
	roots := x509.NewCertPool()

	for _, cert := range rootCAs {
		certContent, err := cert.Read()
		if err != nil {
			log.Error("Error while read RootCAs", err)
			continue
		}
		roots.AppendCertsFromPEM(certContent)
	}

	return roots
}

// Start starts the server.
func (server *Server) Start() {
	server.startHTTPServers()
	server.startLeadership()
	server.routinesPool.Go(func(stop chan bool) {
		server.listenProviders(stop)
	})
	server.routinesPool.Go(func(stop chan bool) {
		server.listenConfigurations(stop)
	})
	server.configureProviders()
	server.startProviders()
	go server.listenSignals()
}

// Wait blocks until server is shutted down.
func (server *Server) Wait() {
	<-server.stopChan
}

// Stop stops the server
func (server *Server) Stop() {
	defer log.Info("Server stopped")
	var wg sync.WaitGroup
	for sepn, sep := range server.serverEntryPoints {
		wg.Add(1)
		go func(serverEntryPointName string, serverEntryPoint *serverEntryPoint) {
			defer wg.Done()
			graceTimeOut := time.Duration(server.globalConfiguration.LifeCycle.GraceTimeOut)
			ctx, cancel := context.WithTimeout(context.Background(), graceTimeOut)
			log.Debugf("Waiting %s seconds before killing connections on entrypoint %s...", graceTimeOut, serverEntryPointName)
			if err := serverEntryPoint.httpServer.Shutdown(ctx); err != nil {
				log.Debugf("Wait is over due to: %s", err)
				serverEntryPoint.httpServer.Close()
			}
			cancel()
			log.Debugf("Entrypoint %s closed", serverEntryPointName)
		}(sepn, sep)
	}
	wg.Wait()
	server.stopChan <- true
}

// Close destroys the server
func (server *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(server.globalConfiguration.LifeCycle.GraceTimeOut))
	go func(ctx context.Context) {
		<-ctx.Done()
		if ctx.Err() == context.Canceled {
			return
		} else if ctx.Err() == context.DeadlineExceeded {
			log.Warn("Timeout while stopping traefik, killing instance ✝")
			os.Exit(1)
		}
	}(ctx)
	stopMetricsClients()
	server.stopLeadership()
	server.routinesPool.Cleanup()
	close(server.configurationChan)
	close(server.configurationValidatedChan)
	signal.Stop(server.signals)
	close(server.signals)
	close(server.stopChan)
	if server.accessLoggerMiddleware != nil {
		if err := server.accessLoggerMiddleware.Close(); err != nil {
			log.Errorf("Error closing access log file: %s", err)
		}
	}
	cancel()
}

func (server *Server) startLeadership() {
	if server.leadership != nil {
		server.leadership.Participate(server.routinesPool)
	}
}

func (server *Server) stopLeadership() {
	if server.leadership != nil {
		server.leadership.Stop()
	}
}

func (server *Server) startHTTPServers() {
	server.serverEntryPoints = server.buildEntryPoints(server.globalConfiguration)

	for newServerEntryPointName, newServerEntryPoint := range server.serverEntryPoints {
		serverEntryPoint := server.setupServerEntryPoint(newServerEntryPointName, newServerEntryPoint)
		go server.startServer(serverEntryPoint, server.globalConfiguration)
	}
}

func (server *Server) setupServerEntryPoint(newServerEntryPointName string, newServerEntryPoint *serverEntryPoint) *serverEntryPoint {
	serverMiddlewares := []negroni.Handler{middlewares.NegroniRecoverHandler()}
	serverInternalMiddlewares := []negroni.Handler{middlewares.NegroniRecoverHandler()}
	if server.accessLoggerMiddleware != nil {
		serverMiddlewares = append(serverMiddlewares, server.accessLoggerMiddleware)
	}
	if server.metricsRegistry.IsEnabled() {
		serverMiddlewares = append(serverMiddlewares, middlewares.NewMetricsWrapper(server.metricsRegistry, newServerEntryPointName))
	}
	if server.globalConfiguration.API != nil {
		server.globalConfiguration.API.Stats = thoas_stats.New()
		serverMiddlewares = append(serverMiddlewares, server.globalConfiguration.API.Stats)
		if server.globalConfiguration.API.Statistics != nil {
			server.globalConfiguration.API.StatsRecorder = middlewares.NewStatsRecorder(server.globalConfiguration.API.Statistics.RecentErrors)
			serverMiddlewares = append(serverMiddlewares, server.globalConfiguration.API.StatsRecorder)
		}

	}
	if server.globalConfiguration.EntryPoints[newServerEntryPointName].Auth != nil {
		authMiddleware, err := mauth.NewAuthenticator(server.globalConfiguration.EntryPoints[newServerEntryPointName].Auth)
		if err != nil {
			log.Fatal("Error starting server: ", err)
		}
		serverMiddlewares = append(serverMiddlewares, authMiddleware)
		serverInternalMiddlewares = append(serverInternalMiddlewares, authMiddleware)
	}
	if server.globalConfiguration.EntryPoints[newServerEntryPointName].Compress {
		serverMiddlewares = append(serverMiddlewares, &middlewares.Compress{})
	}
	if len(server.globalConfiguration.EntryPoints[newServerEntryPointName].WhitelistSourceRange) > 0 {
		ipWhitelistMiddleware, err := middlewares.NewIPWhitelister(server.globalConfiguration.EntryPoints[newServerEntryPointName].WhitelistSourceRange)
		if err != nil {
			log.Fatal("Error starting server: ", err)
		}
		serverMiddlewares = append(serverMiddlewares, ipWhitelistMiddleware)
		serverInternalMiddlewares = append(serverInternalMiddlewares, ipWhitelistMiddleware)
	}
	newSrv, listener, err := server.prepareServer(newServerEntryPointName, server.globalConfiguration.EntryPoints[newServerEntryPointName], newServerEntryPoint.httpRouter, serverMiddlewares, serverInternalMiddlewares)
	if err != nil {
		log.Fatal("Error preparing server: ", err)
	}
	serverEntryPoint := server.serverEntryPoints[newServerEntryPointName]
	serverEntryPoint.httpServer = newSrv
	serverEntryPoint.listener = listener

	return serverEntryPoint
}

func (server *Server) listenProviders(stop chan bool) {
	for {
		select {
		case <-stop:
			return
		case configMsg, ok := <-server.configurationChan:
			if !ok || configMsg.Configuration == nil {
				return
			}
			server.preLoadConfiguration(configMsg)
		}
	}
}

func (server *Server) preLoadConfiguration(configMsg types.ConfigMessage) {
	providerConfigUpdateMap := map[string]chan types.ConfigMessage{}
	providersThrottleDuration := time.Duration(server.globalConfiguration.ProvidersThrottleDuration)
	server.defaultConfigurationValues(configMsg.Configuration)
	currentConfigurations := server.currentConfigurations.Get().(types.Configurations)
	jsonConf, _ := json.Marshal(configMsg.Configuration)
	log.Debugf("Configuration received from provider %s: %s", configMsg.ProviderName, string(jsonConf))
	if configMsg.Configuration == nil || configMsg.Configuration.Backends == nil && configMsg.Configuration.Frontends == nil && configMsg.Configuration.TLSConfiguration == nil {
		log.Infof("Skipping empty Configuration for provider %s", configMsg.ProviderName)
	} else if reflect.DeepEqual(currentConfigurations[configMsg.ProviderName], configMsg.Configuration) {
		log.Infof("Skipping same configuration for provider %s", configMsg.ProviderName)
	} else {
		if _, ok := providerConfigUpdateMap[configMsg.ProviderName]; !ok {
			providerConfigUpdate := make(chan types.ConfigMessage)
			providerConfigUpdateMap[configMsg.ProviderName] = providerConfigUpdate
			server.routinesPool.Go(func(stop chan bool) {
				throttleProviderConfigReload(providersThrottleDuration, server.configurationValidatedChan, providerConfigUpdate, stop)
			})
		}
		providerConfigUpdateMap[configMsg.ProviderName] <- configMsg
	}
}

// throttleProviderConfigReload throttles the configuration reload speed for a single provider.
// It will immediately publish a new configuration and then only publish the next configuration after the throttle duration.
// Note that in the case it receives N new configs in the timeframe of the throttle duration after publishing,
// it will publish the last of the newly received configurations.
func throttleProviderConfigReload(throttle time.Duration, publish chan<- types.ConfigMessage, in <-chan types.ConfigMessage, stop chan bool) {
	ring := channels.NewRingChannel(1)

	safe.Go(func() {
		for {
			select {
			case <-stop:
				return
			case nextConfig := <-ring.Out():
				publish <- nextConfig.(types.ConfigMessage)
				time.Sleep(throttle)
			}
		}
	})

	for {
		select {
		case <-stop:
			return
		case nextConfig := <-in:
			ring.In() <- nextConfig
		}
	}
}

func (server *Server) defaultConfigurationValues(configuration *types.Configuration) {
	if configuration == nil || configuration.Frontends == nil {
		return
	}
	server.configureFrontends(configuration.Frontends)
	server.configureBackends(configuration.Backends)
}

func (server *Server) listenConfigurations(stop chan bool) {
	for {
		select {
		case <-stop:
			return
		case configMsg, ok := <-server.configurationValidatedChan:
			if !ok || configMsg.Configuration == nil {
				return
			}
			server.loadConfiguration(configMsg)
		}
	}
}

// loadConfiguration manages dynamically frontends, backends and TLS configurations
func (server *Server) loadConfiguration(configMsg types.ConfigMessage) {
	currentConfigurations := server.currentConfigurations.Get().(types.Configurations)

	// Copy configurations to new map so we don't change current if LoadConfig fails
	newConfigurations := make(types.Configurations)
	for k, v := range currentConfigurations {
		newConfigurations[k] = v
	}
	newConfigurations[configMsg.ProviderName] = configMsg.Configuration

	newServerEntryPoints, err := server.loadConfig(newConfigurations, server.globalConfiguration)
	if err == nil {
		for newServerEntryPointName, newServerEntryPoint := range newServerEntryPoints {
			server.serverEntryPoints[newServerEntryPointName].httpRouter.UpdateHandler(newServerEntryPoint.httpRouter.GetHandler())
			if &newServerEntryPoint.certs != nil {
				server.serverEntryPoints[newServerEntryPointName].certs.Set(newServerEntryPoint.certs.Get())
			}
			log.Infof("Server configuration reloaded on %s", server.serverEntryPoints[newServerEntryPointName].httpServer.Addr)
		}
		server.currentConfigurations.Set(newConfigurations)
		server.postLoadConfiguration()
	} else {
		log.Error("Error loading new configuration, aborted ", err)
	}
}

// loadHTTPSConfiguration add/delete HTTPS certificate managed dynamically
func (server *Server) loadHTTPSConfiguration(configurations types.Configurations) (map[string]*traefikTls.DomainsCertificates, error) {
	newEPCertificates := make(map[string]*traefikTls.DomainsCertificates)
	// Get all certificates
	for _, configuration := range configurations {
		if configuration.TLSConfiguration != nil && len(configuration.TLSConfiguration) > 0 {
			if err := traefikTls.SortTLSConfigurationPerEntryPoints(configuration.TLSConfiguration, newEPCertificates); err != nil {
				return nil, err
			}
		}
	}
	return newEPCertificates, nil
}

// getCertificate allows to customize tlsConfig.Getcertificate behaviour to get the certificates inserted dynamically
func (s *serverEntryPoint) getCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if s.certs.Get() != nil {
		domainToCheck := types.CanonicalDomain(clientHello.ServerName)
		for domains, cert := range *s.certs.Get().(*traefikTls.DomainsCertificates) {
			for _, domain := range strings.Split(domains, ",") {
				selector := "^" + strings.Replace(domain, "*.", "[^\\.]*\\.?", -1) + "$"
				domainCheck, _ := regexp.MatchString(selector, domainToCheck)
				if domainCheck {
					return cert, nil
				}
			}
		}
	}
	return nil, nil
}

func (server *Server) postLoadConfiguration() {
	if server.globalConfiguration.ACME == nil {
		return
	}
	if server.leadership != nil && !server.leadership.IsLeader() {
		return
	}
	if server.globalConfiguration.ACME.OnHostRule {
		currentConfigurations := server.currentConfigurations.Get().(types.Configurations)
		for _, config := range currentConfigurations {
			for _, frontend := range config.Frontends {

				// check if one of the frontend entrypoints is configured with TLS
				// and is configured with ACME
				ACMEEnabled := false
				for _, entryPoint := range frontend.EntryPoints {
					if server.globalConfiguration.ACME.EntryPoint == entryPoint && server.globalConfiguration.EntryPoints[entryPoint].TLS != nil {
						ACMEEnabled = true
						break
					}
				}

				if ACMEEnabled {
					for _, route := range frontend.Routes {
						rules := Rules{}
						domains, err := rules.ParseDomains(route.Rule)
						if err != nil {
							log.Errorf("Error parsing domains: %v", err)
						} else {
							server.globalConfiguration.ACME.LoadCertificateForDomains(domains)
						}
					}
				}
			}
		}
	}
}

func (server *Server) configureProviders() {
	// configure providers
	if server.globalConfiguration.Docker != nil {
		server.providers = append(server.providers, server.globalConfiguration.Docker)
	}
	if server.globalConfiguration.Marathon != nil {
		server.providers = append(server.providers, server.globalConfiguration.Marathon)
	}
	if server.globalConfiguration.File != nil {
		server.providers = append(server.providers, server.globalConfiguration.File)
	}
	if server.globalConfiguration.Rest != nil {
		server.providers = append(server.providers, server.globalConfiguration.Rest)
		server.globalConfiguration.Rest.CurrentConfigurations = &server.currentConfigurations
	}
	if server.globalConfiguration.Consul != nil {
		server.providers = append(server.providers, server.globalConfiguration.Consul)
	}
	if server.globalConfiguration.ConsulCatalog != nil {
		server.providers = append(server.providers, server.globalConfiguration.ConsulCatalog)
	}
	if server.globalConfiguration.Etcd != nil {
		server.providers = append(server.providers, server.globalConfiguration.Etcd)
	}
	if server.globalConfiguration.Zookeeper != nil {
		server.providers = append(server.providers, server.globalConfiguration.Zookeeper)
	}
	if server.globalConfiguration.Boltdb != nil {
		server.providers = append(server.providers, server.globalConfiguration.Boltdb)
	}
	if server.globalConfiguration.Kubernetes != nil {
		server.providers = append(server.providers, server.globalConfiguration.Kubernetes)
	}
	if server.globalConfiguration.Mesos != nil {
		server.providers = append(server.providers, server.globalConfiguration.Mesos)
	}
	if server.globalConfiguration.Eureka != nil {
		server.providers = append(server.providers, server.globalConfiguration.Eureka)
	}
	if server.globalConfiguration.ECS != nil {
		server.providers = append(server.providers, server.globalConfiguration.ECS)
	}
	if server.globalConfiguration.Rancher != nil {
		server.providers = append(server.providers, server.globalConfiguration.Rancher)
	}
	if server.globalConfiguration.DynamoDB != nil {
		server.providers = append(server.providers, server.globalConfiguration.DynamoDB)
	}
}

func (server *Server) startProviders() {
	// start providers
	for _, p := range server.providers {
		providerType := reflect.TypeOf(p)
		jsonConf, _ := json.Marshal(p)
		log.Infof("Starting provider %v %s", providerType, jsonConf)
		currentProvider := p
		safe.Go(func() {
			err := currentProvider.Provide(server.configurationChan, server.routinesPool, server.globalConfiguration.Constraints)
			if err != nil {
				log.Errorf("Error starting provider %v: %s", providerType, err)
			}
		})
	}
}

func createClientTLSConfig(entryPointName string, tlsOption *traefikTls.TLS) (*tls.Config, error) {
	if tlsOption == nil {
		return nil, errors.New("no TLS provided")
	}

	config, _, err := tlsOption.Certificates.CreateTLSConfig(entryPointName)
	if err != nil {
		return nil, err
	}

	if len(tlsOption.ClientCAFiles) > 0 {
		log.Warnf("Deprecated configuration found during client TLS configuration creation: %s. Please use %s (which allows to make the CA Files optional).", "tls.ClientCAFiles", "tls.ClientCA.files")
		tlsOption.ClientCA.Files = tlsOption.ClientCAFiles
		tlsOption.ClientCA.Optional = false
	}
	if len(tlsOption.ClientCA.Files) > 0 {
		pool := x509.NewCertPool()
		for _, caFile := range tlsOption.ClientCA.Files {
			data, err := ioutil.ReadFile(caFile)
			if err != nil {
				return nil, err
			}
			if !pool.AppendCertsFromPEM(data) {
				return nil, errors.New("invalid certificate(s) in " + caFile)
			}
		}
		config.RootCAs = pool
	}
	config.BuildNameToCertificate()
	return config, nil
}

// creates a TLS config that allows terminating HTTPS for multiple domains using SNI
func (server *Server) createTLSConfig(entryPointName string, tlsOption *traefikTls.TLS, router *middlewares.HandlerSwitcher) (*tls.Config, error) {
	if tlsOption == nil {
		return nil, nil
	}

	config, epDomainsCertificates, err := tlsOption.Certificates.CreateTLSConfig(entryPointName)
	if err != nil {
		return nil, err
	}
	epDomainsCertificatesTmp := new(traefikTls.DomainsCertificates)
	if epDomainsCertificates[entryPointName] != nil {
		epDomainsCertificatesTmp = epDomainsCertificates[entryPointName]
	} else {
		*epDomainsCertificatesTmp = make(map[string]*tls.Certificate)
	}
	server.serverEntryPoints[entryPointName].certs.Set(epDomainsCertificatesTmp)
	// ensure http2 enabled
	config.NextProtos = []string{"h2", "http/1.1"}

	if len(tlsOption.ClientCAFiles) > 0 {
		log.Warnf("Deprecated configuration found during TLS configuration creation: %s. Please use %s (which allows to make the CA Files optional).", "tls.ClientCAFiles", "tls.ClientCA.files")
		tlsOption.ClientCA.Files = tlsOption.ClientCAFiles
		tlsOption.ClientCA.Optional = false
	}
	if len(tlsOption.ClientCA.Files) > 0 {
		pool := x509.NewCertPool()
		for _, caFile := range tlsOption.ClientCA.Files {
			data, err := ioutil.ReadFile(caFile)
			if err != nil {
				return nil, err
			}
			ok := pool.AppendCertsFromPEM(data)
			if !ok {
				return nil, errors.New("invalid certificate(s) in " + caFile)
			}
		}
		config.ClientCAs = pool
		if tlsOption.ClientCA.Optional {
			config.ClientAuth = tls.VerifyClientCertIfGiven
		} else {
			config.ClientAuth = tls.RequireAndVerifyClientCert
		}
	}

	if server.globalConfiguration.ACME != nil {
		if _, ok := server.serverEntryPoints[server.globalConfiguration.ACME.EntryPoint]; ok {
			if entryPointName == server.globalConfiguration.ACME.EntryPoint {
				checkOnDemandDomain := func(domain string) bool {
					routeMatch := &mux.RouteMatch{}
					router := router.GetHandler()
					match := router.Match(&http.Request{URL: &url.URL{}, Host: domain}, routeMatch)
					if match && routeMatch.Route != nil {
						return true
					}
					return false
				}
				if server.leadership == nil {
					err := server.globalConfiguration.ACME.CreateLocalConfig(config, &server.serverEntryPoints[entryPointName].certs, checkOnDemandDomain)
					if err != nil {
						return nil, err
					}
				} else {
					err := server.globalConfiguration.ACME.CreateClusterConfig(server.leadership, config, &server.serverEntryPoints[entryPointName].certs, checkOnDemandDomain)
					if err != nil {
						return nil, err
					}
				}
			}
		} else {
			return nil, errors.New("Unknown entrypoint " + server.globalConfiguration.ACME.EntryPoint + " for ACME configuration")
		}
	} else {
		config.GetCertificate = server.serverEntryPoints[entryPointName].getCertificate
	}
	if len(config.Certificates) == 0 {
		return nil, errors.New("No certificates found for TLS entrypoint " + entryPointName)
	}
	// BuildNameToCertificate parses the CommonName and SubjectAlternateName fields
	// in each certificate and populates the config.NameToCertificate map.
	config.BuildNameToCertificate()
	//Set the minimum TLS version if set in the config TOML
	if minConst, exists := traefikTls.MinVersion[server.globalConfiguration.EntryPoints[entryPointName].TLS.MinVersion]; exists {
		config.PreferServerCipherSuites = true
		config.MinVersion = minConst
	}
	//Set the list of CipherSuites if set in the config TOML
	if server.globalConfiguration.EntryPoints[entryPointName].TLS.CipherSuites != nil {
		//if our list of CipherSuites is defined in the entrypoint config, we can re-initilize the suites list as empty
		config.CipherSuites = make([]uint16, 0)
		for _, cipher := range server.globalConfiguration.EntryPoints[entryPointName].TLS.CipherSuites {
			if cipherConst, exists := traefikTls.CipherSuites[cipher]; exists {
				config.CipherSuites = append(config.CipherSuites, cipherConst)
			} else {
				//CipherSuite listed in the toml does not exist in our listed
				return nil, errors.New("Invalid CipherSuite: " + cipher)
			}
		}
	}
	return config, nil
}

func (server *Server) startServer(serverEntryPoint *serverEntryPoint, globalConfiguration configuration.GlobalConfiguration) {
	log.Infof("Starting server on %s", serverEntryPoint.httpServer.Addr)
	var err error
	if serverEntryPoint.httpServer.TLSConfig != nil {
		err = serverEntryPoint.httpServer.ServeTLS(serverEntryPoint.listener, "", "")
	} else {
		err = serverEntryPoint.httpServer.Serve(serverEntryPoint.listener)
	}
	if err != http.ErrServerClosed {
		log.Error("Error creating server: ", err)
	}
}

func (server *Server) addInternalRoutes(entryPointName string, router *mux.Router) {
	if server.globalConfiguration.Metrics != nil && server.globalConfiguration.Metrics.Prometheus != nil && server.globalConfiguration.Metrics.Prometheus.EntryPoint == entryPointName {
		metrics.PrometheusHandler{}.AddRoutes(router)
	}

	if server.globalConfiguration.Rest != nil && server.globalConfiguration.Rest.EntryPoint == entryPointName {
		server.globalConfiguration.Rest.AddRoutes(router)
	}

	if server.globalConfiguration.API != nil && server.globalConfiguration.API.EntryPoint == entryPointName {
		server.globalConfiguration.API.AddRoutes(router)
	}
}

func (server *Server) addInternalPublicRoutes(entryPointName string, router *mux.Router) {
	if server.globalConfiguration.Ping != nil && server.globalConfiguration.Ping.EntryPoint != "" && server.globalConfiguration.Ping.EntryPoint == entryPointName {
		server.globalConfiguration.Ping.AddRoutes(router)
	}
}

func (server *Server) prepareServer(entryPointName string, entryPoint *configuration.EntryPoint, router *middlewares.HandlerSwitcher, middlewares []negroni.Handler, internalMiddlewares []negroni.Handler) (*http.Server, net.Listener, error) {
	readTimeout, writeTimeout, idleTimeout := buildServerTimeouts(server.globalConfiguration)
	log.Infof("Preparing server %s %+v with readTimeout=%s writeTimeout=%s idleTimeout=%s", entryPointName, entryPoint, readTimeout, writeTimeout, idleTimeout)

	// middlewares
	n := negroni.New()
	for _, middleware := range middlewares {
		n.Use(middleware)
	}
	n.UseHandler(router)

	path := "/"
	if server.globalConfiguration.Web != nil && server.globalConfiguration.Web.Path != "" {
		path = server.globalConfiguration.Web.Path
	}

	internalMuxRouter := server.buildInternalRouter(entryPointName, path, internalMiddlewares)
	internalMuxRouter.NotFoundHandler = n

	tlsConfig, err := server.createTLSConfig(entryPointName, entryPoint.TLS, router)
	if err != nil {
		log.Errorf("Error creating TLS config: %s", err)
		return nil, nil, err
	}

	listener, err := net.Listen("tcp", entryPoint.Address)
	if err != nil {
		log.Error("Error opening listener ", err)
		return nil, nil, err
	}

	if entryPoint.ProxyProtocol != nil {
		IPs, err := whitelist.NewIP(entryPoint.ProxyProtocol.TrustedIPs, entryPoint.ProxyProtocol.Insecure)
		if err != nil {
			return nil, nil, fmt.Errorf("error creating whitelist: %s", err)
		}
		log.Infof("Enabling ProxyProtocol for trusted IPs %v", entryPoint.ProxyProtocol.TrustedIPs)
		listener = &proxyproto.Listener{
			Listener: listener,
			SourceCheck: func(addr net.Addr) (bool, error) {
				ip, ok := addr.(*net.TCPAddr)
				if !ok {
					return false, fmt.Errorf("type error %v", addr)
				}
				return IPs.ContainsIP(ip.IP)
			},
		}
	}

	return &http.Server{
			Addr:         entryPoint.Address,
			Handler:      internalMuxRouter,
			TLSConfig:    tlsConfig,
			ReadTimeout:  readTimeout,
			WriteTimeout: writeTimeout,
			IdleTimeout:  idleTimeout,
		},
		listener,
		nil
}

func (server *Server) buildInternalRouter(entryPointName, path string, internalMiddlewares []negroni.Handler) *mux.Router {
	internalMuxRouter := mux.NewRouter()
	internalMuxRouter.StrictSlash(true)
	internalMuxRouter.SkipClean(true)

	internalMuxSubrouter := internalMuxRouter.PathPrefix(path).Subrouter()
	internalMuxSubrouter.StrictSlash(true)
	internalMuxSubrouter.SkipClean(true)

	server.addInternalRoutes(entryPointName, internalMuxSubrouter)
	internalMuxRouter.Walk(wrapRoute(internalMiddlewares))

	server.addInternalPublicRoutes(entryPointName, internalMuxSubrouter)
	return internalMuxRouter
}

// wrapRoute with middlewares
func wrapRoute(middlewares []negroni.Handler) func(*mux.Route, *mux.Router, []*mux.Route) error {
	return func(route *mux.Route, router *mux.Router, ancestors []*mux.Route) error {
		middles := append(middlewares, negroni.Wrap(route.GetHandler()))
		route.Handler(negroni.New(middles...))
		return nil
	}
}

func buildServerTimeouts(globalConfig configuration.GlobalConfiguration) (readTimeout, writeTimeout, idleTimeout time.Duration) {
	readTimeout = time.Duration(0)
	writeTimeout = time.Duration(0)
	if globalConfig.RespondingTimeouts != nil {
		readTimeout = time.Duration(globalConfig.RespondingTimeouts.ReadTimeout)
		writeTimeout = time.Duration(globalConfig.RespondingTimeouts.WriteTimeout)
	}

	// Prefer legacy idle timeout parameter for backwards compatibility reasons
	if globalConfig.IdleTimeout > 0 {
		idleTimeout = time.Duration(globalConfig.IdleTimeout)
		log.Warn("top-level idle timeout configuration has been deprecated -- please use responding timeouts")
	} else if globalConfig.RespondingTimeouts != nil {
		idleTimeout = time.Duration(globalConfig.RespondingTimeouts.IdleTimeout)
	} else {
		idleTimeout = time.Duration(configuration.DefaultIdleTimeout)
	}

	return readTimeout, writeTimeout, idleTimeout
}

func (server *Server) buildEntryPoints(globalConfiguration configuration.GlobalConfiguration) map[string]*serverEntryPoint {
	serverEntryPoints := make(map[string]*serverEntryPoint)
	for entryPointName := range globalConfiguration.EntryPoints {
		router := server.buildDefaultHTTPRouter()
		serverEntryPoints[entryPointName] = &serverEntryPoint{
			httpRouter: middlewares.NewHandlerSwitcher(router),
		}
	}
	return serverEntryPoints
}

// getRoundTripper will either use server.defaultForwardingRoundTripper or create a new one
// given a custom TLS configuration is passed and the passTLSCert option is set to true.
func (server *Server) getRoundTripper(entryPointName string, globalConfiguration configuration.GlobalConfiguration, passTLSCert bool, tls *traefikTls.TLS) (http.RoundTripper, error) {
	if passTLSCert {
		tlsConfig, err := createClientTLSConfig(entryPointName, tls)
		if err != nil {
			log.Errorf("Failed to create TLSClientConfig: %s", err)
			return nil, err
		}

		transport := createHTTPTransport(globalConfiguration)
		transport.TLSClientConfig = tlsConfig
		return transport, nil
	}

	return server.defaultForwardingRoundTripper, nil
}

// LoadConfig returns a new gorilla.mux Route from the specified global configuration and the dynamic
// provider configurations.
func (server *Server) loadConfig(configurations types.Configurations, globalConfiguration configuration.GlobalConfiguration) (map[string]*serverEntryPoint, error) {
	serverEntryPoints := server.buildEntryPoints(globalConfiguration)
	redirectHandlers := make(map[string]negroni.Handler)
	backends := map[string]http.Handler{}
	backendsHealthCheck := map[string]*healthcheck.BackendHealthCheck{}
	errorHandler := NewRecordingErrorHandler(middlewares.DefaultNetErrorRecorder{})

	for _, config := range configurations {
		frontendNames := sortedFrontendNamesForConfig(config)
	frontend:
		for _, frontendName := range frontendNames {
			frontend := config.Frontends[frontendName]

			log.Debugf("Creating frontend %s", frontendName)

			if len(frontend.EntryPoints) == 0 {
				log.Errorf("No entrypoint defined for frontend %s, defaultEntryPoints:%s", frontendName, globalConfiguration.DefaultEntryPoints)
				log.Errorf("Skipping frontend %s...", frontendName)
				continue frontend
			}

			for _, entryPointName := range frontend.EntryPoints {
				log.Debugf("Wiring frontend %s to entryPoint %s", frontendName, entryPointName)
				if _, ok := serverEntryPoints[entryPointName]; !ok {
					log.Errorf("Undefined entrypoint '%s' for frontend %s", entryPointName, frontendName)
					log.Errorf("Skipping frontend %s...", frontendName)
					continue frontend
				}

				newServerRoute := &serverRoute{route: serverEntryPoints[entryPointName].httpRouter.GetHandler().NewRoute().Name(frontendName)}
				for routeName, route := range frontend.Routes {
					err := getRoute(newServerRoute, &route)
					if err != nil {
						log.Errorf("Error creating route for frontend %s: %v", frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}
					log.Debugf("Creating route %s %s", routeName, route.Rule)
				}

				entryPoint := globalConfiguration.EntryPoints[entryPointName]
				n := negroni.New()
				if entryPoint.Redirect != nil {
					if redirectHandlers[entryPointName] != nil {
						n.Use(redirectHandlers[entryPointName])
					} else if handler, err := server.loadEntryPointConfig(entryPointName, entryPoint); err != nil {
						log.Errorf("Error loading entrypoint configuration for frontend %s: %v", frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					} else {
						if server.accessLoggerMiddleware != nil {
							saveFrontend := accesslog.NewSaveNegroniFrontend(handler, frontendName)
							n.Use(saveFrontend)
							redirectHandlers[entryPointName] = saveFrontend
						} else {
							n.Use(handler)
							redirectHandlers[entryPointName] = handler
						}
					}
				}
				if backends[entryPointName+frontend.Backend] == nil {
					log.Debugf("Creating backend %s", frontend.Backend)

					roundTripper, err := server.getRoundTripper(entryPointName, globalConfiguration, frontend.PassTLSCert, entryPoint.TLS)
					if err != nil {
						log.Errorf("Failed to create RoundTripper for frontend %s: %v", frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}

					rewriter, err := NewHeaderRewriter(entryPoint.ForwardedHeaders.TrustedIPs, entryPoint.ForwardedHeaders.Insecure)
					if err != nil {
						log.Errorf("Error creating rewriter for frontend %s: %v", frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}

					fwd, err := forward.New(
						forward.Logger(oxyLogger),
						forward.PassHostHeader(frontend.PassHostHeader),
						forward.RoundTripper(roundTripper),
						forward.ErrorHandler(errorHandler),
						forward.Rewriter(rewriter),
					)

					if err != nil {
						log.Errorf("Error creating forwarder for frontend %s: %v", frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}

					var rr *roundrobin.RoundRobin
					var saveFrontend http.Handler
					if server.accessLoggerMiddleware != nil {
						saveBackend := accesslog.NewSaveBackend(fwd, frontend.Backend)
						saveFrontend = accesslog.NewSaveFrontend(saveBackend, frontendName)
						rr, _ = roundrobin.New(saveFrontend)
					} else {
						rr, _ = roundrobin.New(fwd)
					}

					if config.Backends[frontend.Backend] == nil {
						log.Errorf("Undefined backend '%s' for frontend %s", frontend.Backend, frontendName)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}

					lbMethod, err := types.NewLoadBalancerMethod(config.Backends[frontend.Backend].LoadBalancer)
					if err != nil {
						log.Errorf("Error loading load balancer method '%+v' for frontend %s: %v", config.Backends[frontend.Backend].LoadBalancer, frontendName, err)
						log.Errorf("Skipping frontend %s...", frontendName)
						continue frontend
					}

					var sticky *roundrobin.StickySession
					var cookieName string
					if stickiness := config.Backends[frontend.Backend].LoadBalancer.Stickiness; stickiness != nil {
						cookieName = cookie.GetName(stickiness.CookieName, frontend.Backend)
						sticky = roundrobin.NewStickySession(cookieName)
					}

					var lb http.Handler
					switch lbMethod {
					case types.Drr:
						log.Debugf("Creating load-balancer drr")
						rebalancer, _ := roundrobin.NewRebalancer(rr, roundrobin.RebalancerLogger(oxyLogger))
						if sticky != nil {
							log.Debugf("Sticky session with cookie %v", cookieName)
							rebalancer, _ = roundrobin.NewRebalancer(rr, roundrobin.RebalancerLogger(oxyLogger), roundrobin.RebalancerStickySession(sticky))
						}
						lb = rebalancer
						if err := configureLBServers(rebalancer, config, frontend); err != nil {
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
						hcOpts := parseHealthCheckOptions(rebalancer, frontend.Backend, config.Backends[frontend.Backend].HealthCheck, globalConfiguration.HealthCheck)
						if hcOpts != nil {
							log.Debugf("Setting up backend health check %s", *hcOpts)
							hcOpts.Transport = server.defaultForwardingRoundTripper
							backendsHealthCheck[entryPointName+frontend.Backend] = healthcheck.NewBackendHealthCheck(*hcOpts)
						}
						lb = middlewares.NewEmptyBackendHandler(rebalancer, lb)
					case types.Wrr:
						log.Debugf("Creating load-balancer wrr")
						if sticky != nil {
							log.Debugf("Sticky session with cookie %v", cookieName)
							if server.accessLoggerMiddleware != nil {
								rr, _ = roundrobin.New(saveFrontend, roundrobin.EnableStickySession(sticky))
							} else {
								rr, _ = roundrobin.New(fwd, roundrobin.EnableStickySession(sticky))
							}
						}
						lb = rr
						if err := configureLBServers(rr, config, frontend); err != nil {
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
						hcOpts := parseHealthCheckOptions(rr, frontend.Backend, config.Backends[frontend.Backend].HealthCheck, globalConfiguration.HealthCheck)
						if hcOpts != nil {
							log.Debugf("Setting up backend health check %s", *hcOpts)
							hcOpts.Transport = server.defaultForwardingRoundTripper
							backendsHealthCheck[entryPointName+frontend.Backend] = healthcheck.NewBackendHealthCheck(*hcOpts)
						}
						lb = middlewares.NewEmptyBackendHandler(rr, lb)
					}

					if len(frontend.Errors) > 0 {
						for _, errorPage := range frontend.Errors {
							if config.Backends[errorPage.Backend] != nil && config.Backends[errorPage.Backend].Servers["error"].URL != "" {
								errorPageHandler, err := middlewares.NewErrorPagesHandler(errorPage, config.Backends[errorPage.Backend].Servers["error"].URL)
								if err != nil {
									log.Errorf("Error creating custom error page middleware, %v", err)
								} else {
									n.Use(errorPageHandler)
								}
							} else {
								log.Errorf("Error Page is configured for Frontend %s, but either Backend %s is not set or Backend URL is missing", frontendName, errorPage.Backend)
							}
						}
					}

					if frontend.RateLimit != nil && len(frontend.RateLimit.RateSet) > 0 {
						lb, err = server.buildRateLimiter(lb, frontend.RateLimit)
						if err != nil {
							log.Errorf("Error creating rate limiter: %v", err)
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
					}

					maxConns := config.Backends[frontend.Backend].MaxConn
					if maxConns != nil && maxConns.Amount != 0 {
						extractFunc, err := utils.NewExtractor(maxConns.ExtractorFunc)
						if err != nil {
							log.Errorf("Error creating connlimit: %v", err)
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
						log.Debugf("Creating load-balancer connlimit")
						lb, err = connlimit.New(lb, extractFunc, maxConns.Amount, connlimit.Logger(oxyLogger))
						if err != nil {
							log.Errorf("Error creating connlimit: %v", err)
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
					}

					if globalConfiguration.Retry != nil {
						countServers := len(config.Backends[frontend.Backend].Servers)
						lb = server.buildRetryMiddleware(lb, globalConfiguration, countServers, frontend.Backend)
					}

					if server.metricsRegistry.IsEnabled() {
						n.Use(middlewares.NewMetricsWrapper(server.metricsRegistry, frontend.Backend))
					}

					ipWhitelistMiddleware, err := configureIPWhitelistMiddleware(frontend.WhitelistSourceRange)
					if err != nil {
						log.Fatalf("Error creating IP Whitelister: %s", err)
					} else if ipWhitelistMiddleware != nil {
						n.Use(ipWhitelistMiddleware)
						log.Infof("Configured IP Whitelists: %s", frontend.WhitelistSourceRange)
					}

					if len(frontend.Redirect) > 0 {
						proto := "http"
						if server.globalConfiguration.EntryPoints[frontend.Redirect].TLS != nil {
							proto = "https"
						}

						regex, replacement, err := server.buildRedirect(proto, entryPoint)
						rewrite, err := middlewares.NewRewrite(regex, replacement, true)
						if err != nil {
							log.Fatalf("Error creating Frontend Redirect: %v", err)
						}
						n.Use(rewrite)
						log.Debugf("Creating frontend %s redirect to %s", frontendName, proto)
					}

					if len(frontend.BasicAuth) > 0 {
						users := types.Users{}
						for _, user := range frontend.BasicAuth {
							users = append(users, user)
						}

						auth := &types.Auth{}
						auth.Basic = &types.Basic{
							Users: users,
						}
						authMiddleware, err := mauth.NewAuthenticator(auth)
						if err != nil {
							log.Errorf("Error creating Auth: %s", err)
						} else {
							n.Use(authMiddleware)
						}
					}

					if frontend.Headers.HasCustomHeadersDefined() {
						headerMiddleware := middlewares.NewHeaderFromStruct(frontend.Headers)
						log.Debugf("Adding header middleware for frontend %s", frontendName)
						n.Use(headerMiddleware)
					}
					if frontend.Headers.HasSecureHeadersDefined() {
						secureMiddleware := middlewares.NewSecure(frontend.Headers)
						log.Debugf("Adding secure middleware for frontend %s", frontendName)
						n.UseFunc(secureMiddleware.HandlerFuncWithNext)
					}

					if config.Backends[frontend.Backend].CircuitBreaker != nil {
						log.Debugf("Creating circuit breaker %s", config.Backends[frontend.Backend].CircuitBreaker.Expression)
						circuitBreaker, err := middlewares.NewCircuitBreaker(lb, config.Backends[frontend.Backend].CircuitBreaker.Expression, cbreaker.Logger(oxyLogger))
						if err != nil {
							log.Errorf("Error creating circuit breaker: %v", err)
							log.Errorf("Skipping frontend %s...", frontendName)
							continue frontend
						}
						n.Use(circuitBreaker)
					} else {
						n.UseHandler(lb)
					}
					backends[entryPointName+frontend.Backend] = n
				} else {
					log.Debugf("Reusing backend %s", frontend.Backend)
				}
				if frontend.Priority > 0 {
					newServerRoute.route.Priority(frontend.Priority)
				}
				server.wireFrontendBackend(newServerRoute, backends[entryPointName+frontend.Backend])

				err := newServerRoute.route.GetError()
				if err != nil {
					log.Errorf("Error building route: %s", err)
				}
			}
		}
	}
	healthcheck.GetHealthCheck().SetBackendsConfiguration(server.routinesPool.Ctx(), backendsHealthCheck)
	// Get new certificates list sorted per entrypoints
	// Update certificates
	entryPointsCertificates, err := server.loadHTTPSConfiguration(configurations)
	//sort routes and update certificates
	for serverEntryPointName, serverEntryPoint := range serverEntryPoints {
		serverEntryPoint.httpRouter.GetHandler().SortRoutes()
		_, exists := entryPointsCertificates[serverEntryPointName]
		if exists {
			serverEntryPoint.certs.Set(entryPointsCertificates[serverEntryPointName])
		}
	}

	return serverEntryPoints, err
}

func configureLBServers(lb healthcheck.LoadBalancer, config *types.Configuration, frontend *types.Frontend) error {
	for serverName, server := range config.Backends[frontend.Backend].Servers {
		u, err := url.Parse(server.URL)
		if err != nil {
			log.Errorf("Error parsing server URL %s: %v", server.URL, err)
			return err
		}
		log.Debugf("Creating server %s at %s with weight %d", serverName, u, server.Weight)
		if err := lb.UpsertServer(u, roundrobin.Weight(server.Weight)); err != nil {
			log.Errorf("Error adding server %s to load balancer: %v", server.URL, err)
			return err
		}
	}
	return nil
}

func configureIPWhitelistMiddleware(whitelistSourceRanges []string) (negroni.Handler, error) {
	if len(whitelistSourceRanges) > 0 {
		ipSourceRanges := whitelistSourceRanges
		ipWhitelistMiddleware, err := middlewares.NewIPWhitelister(ipSourceRanges)

		if err != nil {
			return nil, err
		}

		return ipWhitelistMiddleware, nil
	}

	return nil, nil
}

func (server *Server) wireFrontendBackend(serverRoute *serverRoute, handler http.Handler) {
	// path replace - This needs to always be the very last on the handler chain (first in the order in this function)
	// -- Replacing Path should happen at the very end of the Modifier chain, after all the Matcher+Modifiers ran
	if len(serverRoute.replacePath) > 0 {
		handler = &middlewares.ReplacePath{
			Path:    serverRoute.replacePath,
			Handler: handler,
		}
	}

	if len(serverRoute.replacePathRegex) > 0 {
		sp := strings.Split(serverRoute.replacePathRegex, " ")
		if len(sp) == 2 {
			handler = middlewares.NewReplacePathRegexHandler(sp[0], sp[1], handler)
		} else {
			log.Warnf("Invalid syntax for ReplacePathRegex: %s. Separate the regular expression and the replacement by a space.", serverRoute.replacePathRegex)
		}
	}

	// add prefix - This needs to always be right before ReplacePath on the chain (second in order in this function)
	// -- Adding Path Prefix should happen after all *Strip Matcher+Modifiers ran, but before Replace (in case it's configured)
	if len(serverRoute.addPrefix) > 0 {
		handler = &middlewares.AddPrefix{
			Prefix:  serverRoute.addPrefix,
			Handler: handler,
		}
	}

	// strip prefix
	if len(serverRoute.stripPrefixes) > 0 {
		handler = &middlewares.StripPrefix{
			Prefixes: serverRoute.stripPrefixes,
			Handler:  handler,
		}
	}

	// strip prefix with regex
	if len(serverRoute.stripPrefixesRegex) > 0 {
		handler = middlewares.NewStripPrefixRegex(handler, serverRoute.stripPrefixesRegex)
	}

	serverRoute.route.Handler(handler)
}

func (server *Server) loadEntryPointConfig(entryPointName string, entryPoint *configuration.EntryPoint) (negroni.Handler, error) {
	regex := entryPoint.Redirect.Regex
	replacement := entryPoint.Redirect.Replacement
	var err error
	if len(entryPoint.Redirect.EntryPoint) > 0 {
		var protocol = "http"
		if server.globalConfiguration.EntryPoints[entryPoint.Redirect.EntryPoint].TLS != nil {
			protocol = "https"
		}
		regex, replacement, err = server.buildRedirect(protocol, entryPoint)
	}
	rewrite, err := middlewares.NewRewrite(regex, replacement, true)
	if err != nil {
		return nil, err
	}
	log.Debugf("Creating entryPoint redirect %s -> %s : %s -> %s", entryPointName, entryPoint.Redirect.EntryPoint, regex, replacement)

	return rewrite, nil
}

func (server *Server) buildRedirect(protocol string, entryPoint *configuration.EntryPoint) (string, string, error) {
	regex := `^(?:https?:\/\/)?([\w\._-]+)(?::\d+)?(.*)$`
	if server.globalConfiguration.EntryPoints[entryPoint.Redirect.EntryPoint] == nil {
		return "", "", fmt.Errorf("unknown target entrypoint %q", entryPoint.Redirect.EntryPoint)
	}
	r, _ := regexp.Compile(`(:\d+)`)
	match := r.FindStringSubmatch(server.globalConfiguration.EntryPoints[entryPoint.Redirect.EntryPoint].Address)
	if len(match) == 0 {
		return "", "", fmt.Errorf("bad Address format %q", server.globalConfiguration.EntryPoints[entryPoint.Redirect.EntryPoint].Address)
	}
	replacement := protocol + "://$1" + match[0] + "$2"
	return regex, replacement, nil
}

func (server *Server) buildDefaultHTTPRouter() *mux.Router {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFoundHandler)
	router.StrictSlash(true)
	router.SkipClean(true)
	return router
}

func parseHealthCheckOptions(lb healthcheck.LoadBalancer, backend string, hc *types.HealthCheck, hcConfig *configuration.HealthCheckConfig) *healthcheck.Options {
	if hc == nil || hc.Path == "" || hcConfig == nil {
		return nil
	}

	interval := time.Duration(hcConfig.Interval)
	if hc.Interval != "" {
		intervalOverride, err := time.ParseDuration(hc.Interval)
		switch {
		case err != nil:
			log.Errorf("Illegal healthcheck interval for backend '%s': %s", backend, err)
		case intervalOverride <= 0:
			log.Errorf("Healthcheck interval smaller than zero for backend '%s', backend", backend)
		default:
			interval = intervalOverride
		}
	}

	return &healthcheck.Options{
		Path:     hc.Path,
		Port:     hc.Port,
		Interval: interval,
		LB:       lb,
	}
}

func getRoute(serverRoute *serverRoute, route *types.Route) error {
	rules := Rules{route: serverRoute}
	newRoute, err := rules.Parse(route.Rule)
	if err != nil {
		return err
	}
	newRoute.Priority(serverRoute.route.GetPriority() + len(route.Rule))
	serverRoute.route = newRoute
	return nil
}

func sortedFrontendNamesForConfig(configuration *types.Configuration) []string {
	keys := []string{}
	for key := range configuration.Frontends {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (server *Server) configureFrontends(frontends map[string]*types.Frontend) {
	for _, frontend := range frontends {
		// default endpoints if not defined in frontends
		if len(frontend.EntryPoints) == 0 {
			frontend.EntryPoints = server.globalConfiguration.DefaultEntryPoints
		}
	}
}

func (*Server) configureBackends(backends map[string]*types.Backend) {
	for backendName := range backends {
		backend := backends[backendName]
		if backend.LoadBalancer != nil && backend.LoadBalancer.Sticky {
			log.Warnf("Deprecated configuration found: %s. Please use %s.", "backend.LoadBalancer.Sticky", "backend.LoadBalancer.Stickiness")
		}

		_, err := types.NewLoadBalancerMethod(backend.LoadBalancer)
		if err == nil {
			if backend.LoadBalancer != nil && backend.LoadBalancer.Stickiness == nil && backend.LoadBalancer.Sticky {
				backend.LoadBalancer.Stickiness = &types.Stickiness{
					CookieName: "_TRAEFIK_BACKEND",
				}
			}
		} else {
			log.Debugf("Validation of load balancer method for backend %s failed: %s. Using default method wrr.", backendName, err)

			var stickiness *types.Stickiness
			if backend.LoadBalancer != nil {
				if backend.LoadBalancer.Stickiness == nil {
					if backend.LoadBalancer.Sticky {
						stickiness = &types.Stickiness{
							CookieName: "_TRAEFIK_BACKEND",
						}
					}
				} else {
					stickiness = backend.LoadBalancer.Stickiness
				}
			}
			backend.LoadBalancer = &types.LoadBalancer{
				Method:     "wrr",
				Stickiness: stickiness,
			}
		}
	}
}

func (server *Server) registerMetricClients(metricsConfig *types.Metrics) {
	registries := []metrics.Registry{}

	if metricsConfig.Prometheus != nil {
		registries = append(registries, metrics.RegisterPrometheus(metricsConfig.Prometheus))
		log.Debug("Configured Prometheus metrics")
	}
	if metricsConfig.Datadog != nil {
		registries = append(registries, metrics.RegisterDatadog(metricsConfig.Datadog))
		log.Debugf("Configured DataDog metrics pushing to %s once every %s", metricsConfig.Datadog.Address, metricsConfig.Datadog.PushInterval)
	}
	if metricsConfig.StatsD != nil {
		registries = append(registries, metrics.RegisterStatsd(metricsConfig.StatsD))
		log.Debugf("Configured StatsD metrics pushing to %s once every %s", metricsConfig.StatsD.Address, metricsConfig.StatsD.PushInterval)
	}
	if metricsConfig.InfluxDB != nil {
		registries = append(registries, metrics.RegisterInfluxDB(metricsConfig.InfluxDB))
		log.Debugf("Configured InfluxDB metrics pushing to %s once every %s", metricsConfig.InfluxDB.Address, metricsConfig.InfluxDB.PushInterval)
	}

	if len(registries) > 0 {
		server.metricsRegistry = metrics.NewMultiRegistry(registries)
	}
}

func stopMetricsClients() {
	metrics.StopDatadog()
	metrics.StopStatsd()
	metrics.StopInfluxDB()
}

func (server *Server) buildRateLimiter(handler http.Handler, rlConfig *types.RateLimit) (http.Handler, error) {
	extractFunc, err := utils.NewExtractor(rlConfig.ExtractorFunc)
	if err != nil {
		return nil, err
	}
	log.Debugf("Creating load-balancer rate limiter")
	rateSet := ratelimit.NewRateSet()
	for _, rate := range rlConfig.RateSet {
		if err := rateSet.Add(time.Duration(rate.Period), rate.Average, rate.Burst); err != nil {
			return nil, err
		}
	}
	return ratelimit.New(handler, extractFunc, rateSet, ratelimit.Logger(oxyLogger))
}

func (server *Server) buildRetryMiddleware(handler http.Handler, globalConfig configuration.GlobalConfiguration, countServers int, backendName string) http.Handler {
	retryListeners := middlewares.RetryListeners{}
	if server.metricsRegistry.IsEnabled() {
		retryListeners = append(retryListeners, middlewares.NewMetricsRetryListener(server.metricsRegistry, backendName))
	}
	if server.accessLoggerMiddleware != nil {
		retryListeners = append(retryListeners, &accesslog.SaveRetries{})
	}

	retryAttempts := countServers
	if globalConfig.Retry.Attempts > 0 {
		retryAttempts = globalConfig.Retry.Attempts
	}

	log.Debugf("Creating retries max attempts %d", retryAttempts)

	return middlewares.NewRetry(retryAttempts, handler, retryListeners)
}
