// Copyright 2019 the Service Reflector authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/oklog/run"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	"k8s.io/apiserver/pkg/server"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/squat/service-reflector/pkg/apiserver"
	"github.com/squat/service-reflector/pkg/controller"
	"github.com/squat/service-reflector/pkg/version"
)

const (
	logLevelAll   = "all"
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
	logLevelNone  = "none"
)

var (
	availableLogLevels = strings.Join([]string{
		logLevelAll,
		logLevelDebug,
		logLevelInfo,
		logLevelWarn,
		logLevelError,
		logLevelNone,
	}, ", ")
)

type urls []*url.URL

func (u *urls) String() string {
	us := make([]string, len(*u))
	for i := range *u {
		us[i] = (*u)[i].String()
	}
	return strings.Join(us, ", ")
}

func (u *urls) Set(v string) error {
	for _, raw := range strings.Split(v, ",") {
		trimmed := strings.TrimSpace(raw)
		if len(trimmed) == 0 {
			continue
		}
		uu, err := url.Parse(trimmed)
		if err != nil {
			return err
		}
		*u = append(*u, uu)
	}
	return nil
}

func (u *urls) Type() string {
	return "url"
}

type arrayVar []string

func (a *arrayVar) String() string {
	s := make([]string, len(*a))
	for i := range *a {
		s[i] = (*a)[i]
	}
	return strings.Join(s, ", ")
}

func (a *arrayVar) Set(v string) error {
	*a = append(*a, v)
	return nil
}

func (a *arrayVar) Type() string {
	return "string"
}

// Main is the principal function for the binary, wrapped only by `main` for convenience.
func Main() error {
	kubeconfig := flag.String("kubeconfig", "", "Path to kubeconfig.")
	listen := flag.String("listen", ":9090", "The address at which to listen for health and metrics.")
	logLevel := flag.String("log-level", logLevelInfo, fmt.Sprintf("Log level to use. Possible values: %s", availableLogLevels))
	var apis urls
	namespace := flag.String("namespace", metav1.NamespaceAll, "Namespace to watch for Services.")
	runEmitter := flag.Bool("emitter", true, "Run the Service-Emitter.")
	emitterSelector := flag.String("emitter.selector", "", "Selector to limit what services are emitted app.kubernetes.io/name=foo")
	runReflector := flag.Bool("reflector", true, "Run the Service-Reflector.")
	flag.Var(&apis, "reflector.source-api", "The address of a Kubernetes API server from which to reflect Services.")
	var apiKubeconfigs arrayVar
	flag.Var(&apiKubeconfigs, "reflector.source-kubeconfig", "Path to a Kubeconfig for a Kubernetes API server from which to reflect Services.")
	reflectorSelector := flag.String("reflector.selector", "", "Selector to limit what services are reflected locally, e.g. app.kubernetes.io/name=foo")
	printVersion := flag.Bool("version", false, "Print version and exit")
	secure := genericoptions.NewSecureServingOptions().WithLoopback()
	insecure := genericoptions.DeprecatedInsecureServingOptions{}
	insecure.AddFlags(flag.CommandLine)
	secure.AddFlags(flag.CommandLine)
	flag.Parse()

	if err := secure.Validate(); len(err) != 0 {
		return multiError(err)
	}

	if err := insecure.Validate(); len(err) != 0 {
		return multiError(err)
	}

	if *printVersion {
		fmt.Println(version.Version)
		return nil
	}

	eSelector := labels.Everything()
	if *emitterSelector != "" {
		emitterSelectorSet, err := labels.ConvertSelectorToLabelsMap(*emitterSelector)
		if err != nil {
			return err
		}
		eSelector = emitterSelectorSet.AsSelector()
	}

	logger := log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	switch *logLevel {
	case logLevelAll:
		logger = level.NewFilter(logger, level.AllowAll())
	case logLevelDebug:
		logger = level.NewFilter(logger, level.AllowDebug())
	case logLevelInfo:
		logger = level.NewFilter(logger, level.AllowInfo())
	case logLevelWarn:
		logger = level.NewFilter(logger, level.AllowWarn())
	case logLevelError:
		logger = level.NewFilter(logger, level.AllowError())
	case logLevelNone:
		logger = level.NewFilter(logger, level.AllowNone())
	default:
		return fmt.Errorf("log level %v unknown; possible values are: %s", *logLevel, availableLogLevels)
	}
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "caller", log.DefaultCaller)

	r := prometheus.NewRegistry()
	r.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		return fmt.Errorf("failed to create Kubernetes config: %v", err)
	}
	client := kubernetes.NewForConfigOrDie(config)
	factory := informers.NewSharedInformerFactoryWithOptions(client, 5*time.Minute, informers.WithNamespace(*namespace), informers.WithTweakListOptions(func(*metav1.ListOptions) {}))

	var g run.Group
	{
		// Start the informer factory.
		stop := make(chan struct{})
		g.Add(func() error {
			factory.Core().V1().Endpoints().Informer()
			factory.Core().V1().Services().Informer()
			logger.Log("msg", "starting informers")
			factory.Start(stop)
			syncs := factory.WaitForCacheSync(stop)
			if !syncs[reflect.TypeOf(&v1.Service{})] || !syncs[reflect.TypeOf(&v1.Endpoints{})] {
				return errors.New("failed to sync informer caches")
			}
			logger.Log("msg", "successfully synced informer caches")
			<-stop
			return nil
		}, func(error) {
			close(stop)
		})
	}
	if *runEmitter {
		// Configure the service-emitter.
		insecureInfo := &server.DeprecatedInsecureServingInfo{}
		if err := insecure.ApplyTo(&insecureInfo); err != nil {
			return err
		}
		if err := secure.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
			return fmt.Errorf("error creating self-signed certificates: %v", err)
		}
		cfg := &apiserver.Config{
			GenericConfig: genericapiserver.NewConfig(apiserver.Codecs),
			ExtraConfig: apiserver.ExtraConfig{
				Selector: eSelector,
			},
		}
		if err := secure.ApplyTo(&cfg.GenericConfig.SecureServing, &cfg.GenericConfig.LoopbackClientConfig); err != nil {
			return err
		}
		emitter, err := cfg.Complete(factory).New()
		if err != nil {
			return err
		}

		{
			// Run the service-emitter.
			stop := make(chan struct{})
			g.Add(func() error {
				logger.Log("msg", "starting emitter on HTTPS")
				return emitter.GenericAPIServer.PrepareRun().Run(stop)
			}, func(error) {
				close(stop)
			})
		}
		if insecureInfo.Listener != nil {
			// Run the insecure service-emitter.
			stop := make(chan struct{})
			handler := genericapifilters.WithRequestInfo(emitter.GenericAPIServer.UnprotectedHandler(), server.NewRequestInfoResolver(cfg.GenericConfig))
			g.Add(func() error {
				logger.Log("msg", "starting emitter on HTTP")
				if err := insecureInfo.Serve(handler, cfg.GenericConfig.RequestTimeout, stop); err != nil {
					return err
				}
				<-stop
				return nil
			}, func(error) {
				close(stop)
			})
		}
	}
	{
		// Run the HTTP server.
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.Handle("/metrics", promhttp.HandlerFor(r, promhttp.HandlerOpts{}))
		l, err := net.Listen("tcp", *listen)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %v", *listen, err)
		}

		g.Add(func() error {
			logger.Log("msg", "starting metrics server")
			if err := http.Serve(l, mux); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("error: server exited unexpectedly: %v", err)
			}
			return nil
		}, func(error) {
			l.Close()
		})
	}
	if *runReflector && len(apis)+len(apiKubeconfigs) != 0 {
		// Configure the controller.
		clients := make([]*controller.NamedClient, len(apis)+len(apiKubeconfigs))
		for i := range apis {
			config, err := clientcmd.BuildConfigFromFlags(apis[i].String(), "")
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes config: %v", err)
			}
			clients[i] = &controller.NamedClient{Name: config.Host, Client: kubernetes.NewForConfigOrDie(config)}
		}
		for i := range apiKubeconfigs {
			config, err := clientcmd.BuildConfigFromFlags("", apiKubeconfigs[i])
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes config: %v", err)
			}
			clients[i] = &controller.NamedClient{Name: config.Host, Client: kubernetes.NewForConfigOrDie(config)}
		}
		c := controller.New(client, factory, clients, *namespace, *reflectorSelector, log.With(logger, "component", "controller"))
		c.RegisterMetrics(r)

		// Run the controller.
		stop := make(chan struct{})
		g.Add(func() error {
			logger.Log("msg", "starting reflector")
			c.Run(stop)
			return nil
		}, func(error) {
			close(stop)
		})
	}
	{
		// Exit gracefully on SIGINT and SIGTERM.
		term := make(chan os.Signal, 1)
		signal.Notify(term, syscall.SIGINT, syscall.SIGTERM)
		cancel := make(chan struct{})
		g.Add(func() error {
			for {
				select {
				case <-term:
					logger.Log("msg", "caught interrupt; gracefully cleaning up; see you next time!")
					return nil
				case <-cancel:
					return nil
				}
			}
		}, func(error) {
			close(cancel)
		})
	}

	return g.Run()
}

func main() {
	if err := Main(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

type multiError []error

func (m multiError) Error() string {
	errs := make([]string, len(m))
	for i, err := range m {
		errs[i] = err.Error()
	}
	return strings.Join(errs, ",")
}
