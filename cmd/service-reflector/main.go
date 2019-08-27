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
	"github.com/spf13/pflag"
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

type options struct {
	kubeconfig   string
	listen       string
	logLevel     string
	namespace    string
	printVersion bool

	runEmitter         bool
	emitterSelectorRaw string
	secure             *genericoptions.SecureServingOptionsWithLoopback
	insecure           *genericoptions.DeprecatedInsecureServingOptions

	runReflector         bool
	apis                 urls
	apiKubeconfigs       []string
	reflectorSelectorRaw string

	// Completed fields
	client                   kubernetes.Interface
	factory                  informers.SharedInformerFactory
	logger                   log.Logger
	emitterSelector          labels.Selector
	insecureServingInfo      *server.DeprecatedInsecureServingInfo
	apiserverConfig          *apiserver.Config
	apiserverCompletedConfig apiserver.CompletedConfig
}

func newOptions() *options {
	return &options{
		secure: (&genericoptions.SecureServingOptions{
			BindAddress: net.ParseIP("0.0.0.0"),
			BindPort:    6443,
			ServerCert: genericoptions.GeneratableKeyCert{
				PairName:      "emitter",
				CertDirectory: "emitter.local.config/certificates",
			},
		}).WithLoopback(),
		insecure: &genericoptions.DeprecatedInsecureServingOptions{BindAddress: net.ParseIP("0.0.0.0"), BindPort: 8080},
	}
}

func (o *options) complete() []error {
	var errs []error

	errs = append(errs, o.secure.Validate()...)
	errs = append(errs, o.insecure.Validate()...)

	o.emitterSelector = labels.Everything()
	if o.emitterSelectorRaw != "" {
		emitterSelectorSet, err := labels.ConvertSelectorToLabelsMap(o.emitterSelectorRaw)
		if err != nil {
			errs = append(errs, err)
		} else {
			o.emitterSelector = emitterSelectorSet.AsSelector()
		}
	}

	if o.reflectorSelectorRaw != "" {
		_, err := labels.ConvertSelectorToLabelsMap(o.reflectorSelectorRaw)
		if err != nil {
			errs = append(errs, err)
		}
	}

	o.logger = log.NewJSONLogger(log.NewSyncWriter(os.Stdout))
	o.logger = log.With(o.logger, "ts", log.DefaultTimestampUTC)
	o.logger = log.With(o.logger, "caller", log.DefaultCaller)
	switch o.logLevel {
	case logLevelAll:
		o.logger = level.NewFilter(o.logger, level.AllowAll())
	case logLevelDebug:
		o.logger = level.NewFilter(o.logger, level.AllowDebug())
	case logLevelInfo:
		o.logger = level.NewFilter(o.logger, level.AllowInfo())
	case logLevelWarn:
		o.logger = level.NewFilter(o.logger, level.AllowWarn())
	case logLevelError:
		o.logger = level.NewFilter(o.logger, level.AllowError())
	case logLevelNone:
		o.logger = level.NewFilter(o.logger, level.AllowNone())
	default:
		errs = append(errs, fmt.Errorf("log level %v unknown; possible values are: %s", o.logLevel, availableLogLevels))
	}

	config, err := clientcmd.BuildConfigFromFlags("", o.kubeconfig)
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to create Kubernetes config: %v", err))
	} else {
		o.client = kubernetes.NewForConfigOrDie(config)
		o.factory = informers.NewSharedInformerFactoryWithOptions(o.client, 5*time.Minute, informers.WithNamespace(o.namespace), informers.WithTweakListOptions(func(*metav1.ListOptions) {}))

	}
	if err := o.completeEmitter(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func (o *options) completeEmitter() error {
	if !o.runEmitter {
		return nil
	}
	o.insecureServingInfo = &server.DeprecatedInsecureServingInfo{}
	if err := o.insecure.ApplyTo(&o.insecureServingInfo); err != nil {
		return err
	}
	if err := o.secure.MaybeDefaultWithSelfSignedCerts("localhost", nil, []net.IP{net.ParseIP("127.0.0.1")}); err != nil {
		return fmt.Errorf("error creating self-signed certificates: %v", err)
	}
	o.apiserverConfig = &apiserver.Config{
		GenericConfig: genericapiserver.NewConfig(apiserver.Codecs),
		ExtraConfig: apiserver.ExtraConfig{
			Selector: o.emitterSelector,
		},
	}
	if err := o.secure.ApplyTo(&o.apiserverConfig.GenericConfig.SecureServing, &o.apiserverConfig.GenericConfig.LoopbackClientConfig); err != nil {
		return err
	}
	o.apiserverCompletedConfig = o.apiserverConfig.Complete(o.factory)
	return nil
}

func (o *options) flagSet() *pflag.FlagSet {
	f := pflag.NewFlagSet("service-reflector", pflag.ExitOnError)
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "Path to kubeconfig.")
	f.StringVar(&o.listen, "listen", ":9090", "The address at which to listen for health and metrics.")
	f.StringVar(&o.logLevel, "log-level", logLevelInfo, fmt.Sprintf("Log level to use. Possible values: %s", availableLogLevels))
	f.StringVar(&o.namespace, "namespace", metav1.NamespaceAll, "Namespace to watch for Services.")
	f.BoolVar(&o.printVersion, "version", false, "Print version and exit")
	f.BoolVar(&o.runEmitter, "emitter", true, "Run the Service-Emitter.")
	f.BoolVar(&o.runReflector, "reflector", true, "Run the Service-Reflector.")

	emitter := pflag.NewFlagSet("emitter", pflag.ExitOnError)
	emitter.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName("emitter." + name)
	})
	emitter.StringVar(&o.emitterSelectorRaw, "selector", "", "Selector to limit what services are emitted app.kubernetes.io/name=foo")

	reflector := pflag.NewFlagSet("reflector", pflag.ExitOnError)
	reflector.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		return pflag.NormalizedName("reflector." + name)
	})
	reflector.Var(&o.apis, "source-api", "The address of a Kubernetes API server from which to reflect Services.")
	reflector.StringArrayVar(&o.apiKubeconfigs, "source-kubeconfig", []string{}, "Path to a Kubeconfig for a Kubernetes API server from which to reflect Services.")
	reflector.StringVar(&o.reflectorSelectorRaw, "selector", "", "Selector to limit what services are reflected locally, e.g. app.kubernetes.io/name=foo")

	o.insecure.AddFlags(emitter)
	o.secure.AddFlags(emitter)
	f.AddFlagSet(emitter)
	f.AddFlagSet(reflector)
	return f
}

// Main is the principal function for the binary, wrapped only by `main` for convenience.
func Main() error {
	o := newOptions()
	o.flagSet().Parse(os.Args[1:])

	if o.printVersion {
		fmt.Println(version.Version)
		return nil
	}

	if errs := o.complete(); len(errs) != 0 {
		return multiError(errs)
	}

	r := prometheus.NewRegistry()
	r.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)

	var g run.Group
	{
		// Start the informer factory.
		stop := make(chan struct{})
		g.Add(func() error {
			o.factory.Core().V1().Endpoints().Informer()
			o.factory.Core().V1().Services().Informer()
			o.factory.Core().V1().Namespaces().Informer()
			o.logger.Log("msg", "starting informers")
			o.factory.Start(stop)
			syncs := o.factory.WaitForCacheSync(stop)
			if !syncs[reflect.TypeOf(&v1.Service{})] || !syncs[reflect.TypeOf(&v1.Endpoints{})] {
				return errors.New("failed to sync informer caches")
			}
			o.logger.Log("msg", "successfully synced informer caches")
			<-stop
			return nil
		}, func(error) {
			close(stop)
		})
	}
	if o.runEmitter {
		// Configure the service-emitter.
		emitter, err := o.apiserverCompletedConfig.New()
		if err != nil {
			return err
		}

		{
			// Run the service-emitter.
			stop := make(chan struct{})
			g.Add(func() error {
				o.logger.Log("msg", "starting emitter on HTTPS")
				return emitter.GenericAPIServer.PrepareRun().Run(stop)
			}, func(error) {
				close(stop)
			})
		}
		if o.insecureServingInfo.Listener != nil {
			// Run the insecure service-emitter.
			stop := make(chan struct{})
			handler := genericapifilters.WithRequestInfo(emitter.GenericAPIServer.UnprotectedHandler(), server.NewRequestInfoResolver(o.apiserverConfig.GenericConfig))
			g.Add(func() error {
				o.logger.Log("msg", "starting emitter on HTTP")
				if err := o.insecureServingInfo.Serve(handler, o.apiserverConfig.GenericConfig.RequestTimeout, stop); err != nil {
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
		l, err := net.Listen("tcp", o.listen)
		if err != nil {
			return fmt.Errorf("failed to listen on %s: %v", o.listen, err)
		}

		g.Add(func() error {
			o.logger.Log("msg", "starting metrics server")
			if err := http.Serve(l, mux); err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("error: server exited unexpectedly: %v", err)
			}
			return nil
		}, func(error) {
			l.Close()
		})
	}
	if o.runReflector && len(o.apis)+len(o.apiKubeconfigs) != 0 {
		// Configure the controller.
		clients := make([]*controller.NamedClient, len(o.apis)+len(o.apiKubeconfigs))
		for i := range o.apis {
			config, err := clientcmd.BuildConfigFromFlags(o.apis[i].String(), "")
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes config: %v", err)
			}
			clients[i] = &controller.NamedClient{Name: config.Host, Client: kubernetes.NewForConfigOrDie(config)}
		}
		for i := range o.apiKubeconfigs {
			config, err := clientcmd.BuildConfigFromFlags("", o.apiKubeconfigs[i])
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes config: %v", err)
			}
			clients[i+len(o.apis)] = &controller.NamedClient{Name: config.Host, Client: kubernetes.NewForConfigOrDie(config)}
		}
		c := controller.New(o.client, o.factory, clients, o.namespace, o.reflectorSelectorRaw, log.With(o.logger, "component", "controller"))
		c.RegisterMetrics(r)

		// Run the controller.
		stop := make(chan struct{})
		g.Add(func() error {
			o.logger.Log("msg", "starting reflector")
			return c.Run(stop)
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
					o.logger.Log("msg", "caught interrupt; gracefully cleaning up; see you next time!")
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
