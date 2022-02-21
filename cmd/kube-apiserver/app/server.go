/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericfeatures "k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/server/filters"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	"k8s.io/apiserver/pkg/util/notfoundhandler"
	"k8s.io/apiserver/pkg/util/openapi"
	"k8s.io/apiserver/pkg/util/webhook"
	clientgoinformers "k8s.io/client-go/informers"
	clientgoclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/workqueue" // for workqueue metric registration
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	netutils "k8s.io/utils/net"

	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme" //引入legacyscheme，内部的init方法实现资源注册表的注册
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/controlplane" // 引入控制面的包，包中的import_known_versions.go调用了k8s资源下的install包，通过导入包触发初始化函数。 每种资源下都定义install包，被引用时触发init函数完成资源注册过程
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/kubernetes/pkg/kubeapiserver"
	kubeapiserveradmission "k8s.io/kubernetes/pkg/kubeapiserver/admission"
	kubeauthenticator "k8s.io/kubernetes/pkg/kubeapiserver/authenticator"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	rbacrest "k8s.io/kubernetes/pkg/registry/rbac/rest"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// NewAPIServerCommand creates a *cobra.Command object with default parameters
func NewAPIServerCommand() *cobra.Command {
	// 初始化各个模块的默认配置，内部调用了各个模块各自的默认配置
	s := options.NewServerRunOptions()
	cmd := &cobra.Command{
		Use: "kube-apiserver",
		Long: `The Kubernetes API server validates and configures data
for the api objects which include pods, services, replicationcontrollers, and
others. The API Server services REST operations and provides the frontend to the
cluster's shared state through which all other components interact.`,

		// stop printing usage when the command errors
		SilenceUsage: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-apiserver loopback clients should not log self-issued warnings.
			rest.SetDefaultWarningHandler(rest.NoWarnings{})
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			verflag.PrintAndExitIfRequested()
			fs := cmd.Flags()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			if err := s.Logs.ValidateAndApply(); err != nil {
				return err
			}
			cliflag.PrintFlags(fs)

			// set default options
			// 设置默认参数配置
			completedOptions, err := Complete(s)
			if err != nil {
				return err
			}

			// validate options
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return utilerrors.NewAggregate(errs)
			}

			// 启动运行，常驻进程；进入
			return Run(completedOptions, genericapiserver.SetupSignalHandler())
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	options.AddCustomGlobalFlags(namedFlagSets.FlagSet("generic"))
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

// Run runs the specified APIServer.  This should never exit.
func Run(completeOptions completedServerRunOptions, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	// 1. 创建服务链（注册3个路由）
	// "CreateServerChain 是完成 server 【初始化】的方法，
	// http server 链中包含 apiserver 要启动的三个 server，以及为每个 server 注册对应资源的路由；
	// 里面包含 APIExtensionsServer、KubeAPIServer、AggregatorServer
	//  初始化的所有流程，最终返回 aggregatorapiserver.APIAggregator 实例，
	//  初始化流程主要有：
	//       - http filter chain 的配置、
	//       - API Group 的注册
	//       - http path 与 handler 的关联以及 handler 后端存储 etcd 的配置。
	//
	// 其主要逻辑为：
	//  1、调用 CreateKubeAPIServerConfig 创建 KubeAPIServer 所需要的配置，
	//       i.主要是创建 master.Config，其中会调用 buildGenericConfig 生成 genericConfig，
	//       ii.genericConfig 中包含 apiserver 的核心配置；
	//  2、判断是否启用了扩展的 API server 并调用 createAPIExtensionsConfig 为其创建配置，
	//     apiExtensions server 是一个代理服务，用于代理 kubeapiserver 中的其他 server，比如 metric-server；
	//  3、调用 createAPIExtensionsServer 创建 apiExtensionsServer 实例；
	//  4、调用 CreateKubeAPIServer 初始化 kubeAPIServer；
	//  5、调用 createAggregatorConfig 为 aggregatorServer 创建配置并调用 createAggregatorServer 初始化 aggregatorServer；
	//  6、配置并判断是否启动非安全的 http server；
	server, err := CreateServerChain(completeOptions, stopCh)
	if err != nil {
		return err
	}

	// 2. 预运行：注册健康检查、就绪、存活探针的地址
	// 调用 server.PrepareRun 进行服务运行前的准备，该方法主要完成了健康检查、存活检查和OpenAPI路由的注册工作；
	prepared, err := server.PrepareRun()
	if err != nil {
		return err
	}

	// 3. 正式运行http server
	// 调用 prepared.Run 启动 https server；
	return prepared.Run(stopCh)
}

// CreateServerChain creates the apiservers connected via delegation.
// 创建服务链
//                    |--> CreateNodeDialer
//                    |
//                    |--> CreateKubeAPIServerConfig
//                    |
//CreateServerChain --|--> createAPIExtensionsConfig
//                    |
//                    |                                                                       |--> c.GenericConfig.New
//                    |--> createAPIExtensionsServer --> apiextensionsConfig.Complete().New --|
//                    |                                                                       |--> s.GenericAPIServer.InstallAPIGroup
//                    |
//                    |                                                                 |--> c.GenericConfig.New
//                    |                                                                 |
//                    |--> CreateKubeAPIServer --> kubeAPIServerConfig.Complete().New --|--> m.InstallLegacyAPI --> legacyRESTStorageProvider.NewLegacyRESTStorage --> m.GenericAPIServer.InstallLegacyAPIGroup
//                    |                                                                 |
//                    |                                                                 |--> m.InstallAPIs --> restStorageBuilder.NewRESTStorage --> m.GenericAPIServer.InstallAPIGroups
//                    |
//                    |
//                    |--> createAggregatorConfig
//                    |
//                    |                                                                             |--> c.GenericConfig.New
//                    |                                                                             |
//                    |--> createAggregatorServer --> aggregatorConfig.Complete().NewWithDelegate --|--> apiservicerest.NewRESTStorage
//                                                                                                  |
//                                                                                                  |--> s.GenericAPIServer.InstallAPIGroup

func CreateServerChain(completedOptions completedServerRunOptions, stopCh <-chan struct{}) (*aggregatorapiserver.APIAggregator, error) {

	// 1.创建 kubeapi-server 配置
	// 1、调用 CreateKubeAPIServerConfig 创建 KubeAPIServer 所需要的配置，
	//    i.主要是创建 master.Config，其中会调用 buildGenericConfig 生成 genericConfig，
	//     genericConfig 中包含 apiserver 的核心配置；
	kubeAPIServerConfig, serviceResolver, pluginInitializer, err := CreateKubeAPIServerConfig(completedOptions)
	if err != nil {
		return nil, err
	}

	// If additional API servers are added, they should be gated.
	// 2.创建 kubeapi-extension-server 配置
	// 2、判断是否启用了扩展的 API server 并调用 createAPIExtensionsConfig 为其创建配置，
	//    apiExtensions server 是一个代理服务，用于代理 kubeapiserver 中的其他 server，比如 metric-server；
	apiExtensionsConfig, err := createAPIExtensionsConfig(
		*kubeAPIServerConfig.GenericConfig,
		kubeAPIServerConfig.ExtraConfig.VersionedInformers,
		pluginInitializer,
		completedOptions.ServerRunOptions,
		completedOptions.MasterCount,
		serviceResolver,
		webhook.NewDefaultAuthenticationInfoResolverWrapper(
			kubeAPIServerConfig.ExtraConfig.ProxyTransport,
			kubeAPIServerConfig.GenericConfig.EgressSelector,
			kubeAPIServerConfig.GenericConfig.LoopbackClientConfig,
			kubeAPIServerConfig.GenericConfig.TracerProvider))
	if err != nil {
		return nil, err
	}

	notFoundHandler := notfoundhandler.New(kubeAPIServerConfig.GenericConfig.Serializer, genericapifilters.NoMuxAndDiscoveryIncompleteKey)

	// 3.创建 kubeapi-extension-server 服务
	// 3、调用 createAPIExtensionsServer 创建 apiExtensionsServer 实例；
	//    1。创建genericApiserver
	//    2。实例化CRD
	//    3。实例化APIGroupInfo
	//    4，installAPIGroup
	apiExtensionsServer, err := createAPIExtensionsServer(apiExtensionsConfig, genericapiserver.NewEmptyDelegateWithCustomHandler(notFoundHandler))
	if err != nil {
		return nil, err
	}

	// 4.创建 kubeapi-server 服务
	// 4、调用 CreateKubeAPIServer初始化 kubeAPIServer；
	//   1。创建genericApiserver
	//   2。实例化instance
	//   3。installLegacyAPI
	//   4。installAPI
	kubeAPIServer, err := CreateKubeAPIServer(kubeAPIServerConfig, apiExtensionsServer.GenericAPIServer)
	if err != nil {
		return nil, err
	}

	// aggregator comes last in the chain
	aggregatorConfig, err := createAggregatorConfig(*kubeAPIServerConfig.GenericConfig, completedOptions.ServerRunOptions, kubeAPIServerConfig.ExtraConfig.VersionedInformers, serviceResolver, kubeAPIServerConfig.ExtraConfig.ProxyTransport, pluginInitializer)
	if err != nil {
		return nil, err
	}

	// 创建 aggregator-server 配置
	// 5、调用 createAggregatorConfig 为 aggregatorServer 创建配置并调用 createAggregatorServer 初始化 aggregatorServer；
	//    1。创建genericApiserver
	//    2。实例化aggregrator
	//    3。实例化APIGroupInfo
	//    4，installAPIGroup
	aggregatorServer, err := createAggregatorServer(aggregatorConfig, kubeAPIServer.GenericAPIServer, apiExtensionsServer.Informers)
	if err != nil {
		// we don't need special handling for innerStopCh because the aggregator server doesn't create any go routines
		return nil, err
	}

	// 以上是对 AggregatorServer 初始化流程的分析，可以看出，在创建 APIExtensionsServer、KubeAPIServer 以及
	// AggregatorServer 时，其模式都是类似的，首先调用 c.GenericConfig.New 按照go-restful的模式初始化 Container，
	// 然后为 server 中需要注册的资源创建 RESTStorage，最后将 resource 的 APIGroup 信息注册到路由中
	return aggregatorServer, nil
}

// CreateKubeAPIServer creates and wires a workable kube-apiserver
// 创建KubeAPIServer的流程与创建KubeAPIExtensionServer的流程类似，原理一样。包括：
//
// - 将与资源存储对象进行映射并存储到APIGroupInfo的map中
// - 通过installer.install安装器为资源注册对应的handlers方法（即资源存储对象的ResourceStorage）
// - 完成资源与handlers方法的绑定，并构造Route添加到WebService
// - 最后将WebService添加到container中
// ---------------------------------------------------
// 1、调用 c.GenericConfig.New 初始化 GenericAPIServer，其主要实现在上文已经分析过；
// 2、判断是否支持 logs 相关的路由，如果支持，则添加 /logs 路由；
// 3、调用 m.InstallLegacyAPI 将核心 API Resource 添加到路由中，对应到 apiserver 就是以 /api 开头的 resource；
// 4、调用 m.InstallAPIs 将扩展的 API Resource 添加到路由中，在 apiserver 中即是以 /apis 开头的 resource；
func CreateKubeAPIServer(kubeAPIServerConfig *controlplane.Config, delegateAPIServer genericapiserver.DelegationTarget) (*controlplane.Instance, error) {
	kubeAPIServer, err := kubeAPIServerConfig.Complete().New(delegateAPIServer)
	if err != nil {
		return nil, err
	}

	return kubeAPIServer, nil
}

// CreateProxyTransport creates the dialer infrastructure to connect to the nodes.
func CreateProxyTransport() *http.Transport {
	var proxyDialerFn utilnet.DialFunc
	// Proxying to pods and services is IP-based... don't expect to be able to verify the hostname
	proxyTLSClientConfig := &tls.Config{InsecureSkipVerify: true}
	proxyTransport := utilnet.SetTransportDefaults(&http.Transport{
		DialContext:     proxyDialerFn,
		TLSClientConfig: proxyTLSClientConfig,
	})
	return proxyTransport
}

// CreateKubeAPIServerConfig creates all the resources for running the API server, but runs none of them
// 1、调用 CreateKubeAPIServerConfig 创建 KubeAPIServer 所需要的配置，
//    i.主要是创建 master.Config，其中会调用 buildGenericConfig 生成 genericConfig，genericConfig 中包含 apiserver 的核心配置；
func CreateKubeAPIServerConfig(s completedServerRunOptions) (
	*controlplane.Config,
	aggregatorapiserver.ServiceResolver,
	[]admission.PluginInitializer,
	error,
) {
	proxyTransport := CreateProxyTransport()

	// 构建通用配置
	// (1)、genericConfig 中包含 apiserver 的核心配置；
	genericConfig,
		versionedInformers,
		serviceResolver,
		pluginInitializers,
		admissionPostStartHook,
		storageFactory,
		err := buildGenericConfig(s.ServerRunOptions, proxyTransport)
	if err != nil {
		return nil, nil, nil, err
	}

	//（2)、初始化所支持的 capabilities
	capabilities.Initialize(capabilities.Capabilities{
		AllowPrivileged: s.AllowPrivileged,
		// TODO(vmarmol): Implement support for HostNetworkSources.
		PrivilegedSources: capabilities.PrivilegedSources{
			HostNetworkSources: []string{},
			HostPIDSources:     []string{},
			HostIPCSources:     []string{},
		},
		PerConnectionBandwidthLimitBytesPerSec: s.MaxConnectionBytesPerSec,
	})

	s.Metrics.Apply()
	serviceaccount.RegisterMetrics()

	//(3)、构造controlplane.Config
	config := &controlplane.Config{
		GenericConfig: genericConfig,
		ExtraConfig: controlplane.ExtraConfig{
			APIResourceConfigSource: storageFactory.APIResourceConfigSource,
			StorageFactory:          storageFactory,
			EventTTL:                s.EventTTL,
			KubeletClientConfig:     s.KubeletConfig,
			EnableLogsSupport:       s.EnableLogsHandler,
			ProxyTransport:          proxyTransport,

			ServiceIPRange:          s.PrimaryServiceClusterIPRange,
			APIServerServiceIP:      s.APIServerServiceIP,
			SecondaryServiceIPRange: s.SecondaryServiceClusterIPRange,

			APIServerServicePort: 443,

			ServiceNodePortRange:      s.ServiceNodePortRange,
			KubernetesServiceNodePort: s.KubernetesServiceNodePort,

			EndpointReconcilerType: reconcilers.Type(s.EndpointReconcilerType),
			MasterCount:            s.MasterCount,

			ServiceAccountIssuer:        s.ServiceAccountIssuer,
			ServiceAccountMaxExpiration: s.ServiceAccountTokenMaxExpiration,
			ExtendExpiration:            s.Authentication.ServiceAccounts.ExtendExpiration,

			VersionedInformers: versionedInformers,

			IdentityLeaseDurationSeconds:      s.IdentityLeaseDurationSeconds,
			IdentityLeaseRenewIntervalSeconds: s.IdentityLeaseRenewIntervalSeconds,
		},
	}

	clientCAProvider, err := s.Authentication.ClientCert.GetClientCAContentProvider()
	if err != nil {
		return nil, nil, nil, err
	}
	config.ExtraConfig.ClusterAuthenticationInfo.ClientCA = clientCAProvider

	requestHeaderConfig, err := s.Authentication.RequestHeader.ToAuthenticationRequestHeaderConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	if requestHeaderConfig != nil {
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderCA = requestHeaderConfig.CAContentProvider
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderAllowedNames = requestHeaderConfig.AllowedClientNames
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderExtraHeaderPrefixes = requestHeaderConfig.ExtraHeaderPrefixes
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderGroupHeaders = requestHeaderConfig.GroupHeaders
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderUsernameHeaders = requestHeaderConfig.UsernameHeaders
	}

	if err := config.GenericConfig.AddPostStartHook("start-kube-apiserver-admission-initializer", admissionPostStartHook); err != nil {
		return nil, nil, nil, err
	}

	if config.GenericConfig.EgressSelector != nil {
		// Use the config.GenericConfig.EgressSelector lookup to find the dialer to connect to the kubelet
		config.ExtraConfig.KubeletClientConfig.Lookup = config.GenericConfig.EgressSelector.Lookup

		// Use the config.GenericConfig.EgressSelector lookup as the transport used by the "proxy" subresources.
		networkContext := egressselector.Cluster.AsNetworkContext()
		dialer, err := config.GenericConfig.EgressSelector.Lookup(networkContext)
		if err != nil {
			return nil, nil, nil, err
		}
		c := proxyTransport.Clone()
		c.DialContext = dialer
		config.ExtraConfig.ProxyTransport = c
	}

	// Load the public keys.
	var pubKeys []interface{}
	for _, f := range s.Authentication.ServiceAccounts.KeyFiles {
		keys, err := keyutil.PublicKeysFromFile(f)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse key file %q: %v", f, err)
		}
		pubKeys = append(pubKeys, keys...)
	}
	// Plumb the required metadata through ExtraConfig.
	config.ExtraConfig.ServiceAccountIssuerURL = s.Authentication.ServiceAccounts.Issuers[0]
	config.ExtraConfig.ServiceAccountJWKSURI = s.Authentication.ServiceAccounts.JWKSURI
	config.ExtraConfig.ServiceAccountPublicKeys = pubKeys

	return config, serviceResolver, pluginInitializers, nil
}

// BuildGenericConfig takes the master server options and produces the genericapiserver.Config associated with it
// buildGenericConfig:
// 1、调用 genericapiserver.NewConfig 生成默认的 genericConfig，genericConfig 中主要配置了 DefaultBuildHandlerChain，DefaultBuildHandlerChain 中包含了认证、鉴权等一系列 http filter chain；
// 2、调用 master.DefaultAPIResourceConfigSource 加载需要启用的 API Resource，集群中所有的 API Resource 可以在代码的 k8s.io/api 目录中看到，随着版本的迭代也会不断变化；
// 3、为 genericConfig 中的部分字段设置默认值；
// 4、调用 completedStorageFactoryConfig.New 创建 storageFactory，后面会使用 storageFactory 为每种API Resource 创建对应的 RESTStorage；
func buildGenericConfig(
	s *options.ServerRunOptions,
	proxyTransport *http.Transport,
) (
	genericConfig *genericapiserver.Config,
	versionedInformers clientgoinformers.SharedInformerFactory,
	serviceResolver aggregatorapiserver.ServiceResolver,
	pluginInitializers []admission.PluginInitializer,
	admissionPostStartHook genericapiserver.PostStartHookFunc,
	storageFactory *serverstorage.DefaultStorageFactory,
	lastErr error,
) {

	// 1、为 genericConfig 设置默认值
	// genericConfig 中主要配置了 DefaultBuildHandlerChain，DefaultBuildHandlerChain 中包含了认证、鉴权等一系列 http filter chain
	genericConfig = genericapiserver.NewConfig(legacyscheme.Codecs)

	// 2、配置启动、禁用的GV(group version)
	// 加载需要启用的 API Resource，
	// 集群中所有的 API Resource 可以在代码的 k8s.io/api 目录中看到，随着版本的迭代也会不断变化
	genericConfig.MergedResourceConfig = controlplane.DefaultAPIResourceConfigSource()

	if lastErr = s.GenericServerRunOptions.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	if lastErr = s.SecureServing.ApplyTo(&genericConfig.SecureServing, &genericConfig.LoopbackClientConfig); lastErr != nil {
		return
	}
	if lastErr = s.Features.ApplyTo(genericConfig); lastErr != nil {
		return
	}
	if lastErr = s.APIEnablement.ApplyTo(genericConfig, controlplane.DefaultAPIResourceConfigSource(), legacyscheme.Scheme); lastErr != nil {
		return
	}
	if lastErr = s.EgressSelector.ApplyTo(genericConfig); lastErr != nil {
		return
	}
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		if lastErr = s.Traces.ApplyTo(genericConfig.EgressSelector, genericConfig); lastErr != nil {
			return
		}
	}

	// wrap the definitions to revert any changes from disabled features
	// openapi/swagger配置
	// OpenAPIConfig用于生成OpenAPI规范
	getOpenAPIDefinitions := openapi.GetOpenAPIDefinitionsWithoutDisabledFeatures(generatedopenapi.GetOpenAPIDefinitions)
	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getOpenAPIDefinitions, openapinamer.NewDefinitionNamer(legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme))
	genericConfig.OpenAPIConfig.Info.Title = "Kubernetes"
	genericConfig.LongRunningFunc = filters.BasicLongRunningRequestCheck(
		sets.NewString("watch", "proxy"),
		sets.NewString("attach", "exec", "proxy", "log", "portforward"),
	)

	kubeVersion := version.Get()
	genericConfig.Version = &kubeVersion

	// 3、etcd配置
	// storageFactoryConfig对象定义了kube-apiserver与etcd的交互方式，如：etcd认证、地址、存储前缀等
	// 该对象也定义了资源存储方式，如：资源信息、资源编码信息、资源状态等
	storageFactoryConfig := kubeapiserver.NewStorageFactoryConfig()
	storageFactoryConfig.APIResourceConfig = genericConfig.MergedResourceConfig
	completedStorageFactoryConfig, err := storageFactoryConfig.Complete(s.Etcd)
	if err != nil {
		lastErr = err
		return
	}
	// 初始化 storageFactory
	// 后面会使用 storageFactory 为每种API Resource 创建对应的 RESTStorage；
	storageFactory, lastErr = completedStorageFactoryConfig.New()
	if lastErr != nil {
		return
	}
	if genericConfig.EgressSelector != nil {
		storageFactory.StorageConfig.Transport.EgressLookup = genericConfig.EgressSelector.Lookup
	}
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) && genericConfig.TracerProvider != nil {
		storageFactory.StorageConfig.Transport.TracerProvider = genericConfig.TracerProvider
	}

	// 初始化 RESTOptionsGetter，后期根据其获取操作 Etcd 的句柄，同时添加 etcd 的健康检查方法
	if lastErr = s.Etcd.ApplyWithStorageFactoryTo(storageFactory, genericConfig); lastErr != nil {
		return
	}

	// Use protobufs for self-communication.
	// Since not every generic apiserver has to support protobufs, we
	// cannot default to it in generic apiserver and need to explicitly
	// set it in kube-apiserver.
	// 设置使用 protobufs 用来内部交互，并且禁用压缩功能
	genericConfig.LoopbackClientConfig.ContentConfig.ContentType = "application/vnd.kubernetes.protobuf"

	// Disable compression for self-communication, since we are going to be
	// on a fast local network
	genericConfig.LoopbackClientConfig.DisableCompression = true

	// 4、创建 clientset, 监听127.0.0.1的client
	kubeClientConfig := genericConfig.LoopbackClientConfig
	clientgoExternalClient, err := clientgoclientset.NewForConfig(kubeClientConfig)
	if err != nil {
		lastErr = fmt.Errorf("failed to create real external clientset: %v", err)
		return
	}
	// NewSharedInformerFactory初始化
	versionedInformers = clientgoinformers.NewSharedInformerFactory(clientgoExternalClient, 10*time.Minute)

	// Authentication.ApplyTo requires already applied OpenAPIConfig and EgressSelector if present
	// 5、认证配置
	// 内部调用 authenticatorConfig.New()
	// k8s提供9种认证机制，每种认证机制被实例化后都成为认证器
	// 【创建认证实例】，支持多种认证方式：请求 Header 认证、Auth 文件认证、CA 证书认证、Bearer token 认证、
	// ServiceAccount 认证、BootstrapToken 认证、WebhookToken 认证等
	// 用后5个来设置第一个
	if lastErr = s.Authentication.ApplyTo(
		&genericConfig.Authentication,
		genericConfig.SecureServing,
		genericConfig.EgressSelector,
		genericConfig.OpenAPIConfig,
		clientgoExternalClient,
		versionedInformers,
	); lastErr != nil {
		return
	}

	// 6、授权配置
	// k8s提供6种授权机制，每种授权机制被实例化后都成为授权器
	// 【创建鉴权实例】，包含：Node、RBAC、Webhook、ABAC、AlwaysAllow、AlwaysDeny
	genericConfig.Authorization.Authorizer, genericConfig.RuleResolver, err = BuildAuthorizer(s, genericConfig.EgressSelector, versionedInformers)
	if err != nil {
		lastErr = fmt.Errorf("invalid authorization config: %v", err)
		return
	}
	if !sets.NewString(s.Authorization.Modes...).Has(modes.ModeRBAC) {
		genericConfig.DisabledPostStartHooks.Insert(rbacrest.PostStartHookName)
	}

	// 7、审计插件的初始化
	lastErr = s.Audit.ApplyTo(genericConfig)
	if lastErr != nil {
		return
	}

	// k8s资源在认证和授权通过，被持久化到etcd之前进入准入控制逻辑
	// 准入控制包括：对请求的资源进行自定义操作（校验、修改、拒绝）
	// k8s支持31种准入控制
	// 准入控制器通过Plugins数据结构统一注册、存放、管理
	admissionConfig := &kubeapiserveradmission.Config{
		ExternalInformers:    versionedInformers,
		LoopbackClientConfig: genericConfig.LoopbackClientConfig,
		CloudConfigFile:      s.CloudProvider.CloudConfigFile,
	}
	serviceResolver = buildServiceResolver(
		s.EnableAggregatorRouting,
		genericConfig.LoopbackClientConfig.Host,
		versionedInformers,
	)

	// 8、准入插件的初始化
	pluginInitializers, admissionPostStartHook, err = admissionConfig.New(
		proxyTransport,
		genericConfig.EgressSelector,
		serviceResolver,
		genericConfig.TracerProvider,
	)
	if err != nil {
		lastErr = fmt.Errorf("failed to create admission plugin initializer: %v", err)
		return
	}

	// 准入器admission配置（准入控制器），webhook
	err = s.Admission.ApplyTo(
		genericConfig,
		versionedInformers,
		kubeClientConfig,
		utilfeature.DefaultFeatureGate,
		pluginInitializers...)
	if err != nil {
		lastErr = fmt.Errorf("failed to initialize admission: %v", err)
		return
	}

	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIPriorityAndFairness) && s.GenericServerRunOptions.EnablePriorityAndFairness {
		genericConfig.FlowControl, lastErr = BuildPriorityAndFairness(s, clientgoExternalClient, versionedInformers)
	}

	return
}

// BuildAuthorizer constructs the authorizer
// 授权初始化
func BuildAuthorizer(
	s *options.ServerRunOptions,
	EgressSelector *egressselector.EgressSelector,
	versionedInformers clientgoinformers.SharedInformerFactory) (authorizer.Authorizer, authorizer.RuleResolver, error) {
	authorizationConfig := s.Authorization.ToAuthorizationConfig(versionedInformers)

	if EgressSelector != nil {
		egressDialer, err := EgressSelector.Lookup(egressselector.ControlPlane.AsNetworkContext())
		if err != nil {
			return nil, nil, err
		}
		authorizationConfig.CustomDial = egressDialer
	}

	// 进入
	return authorizationConfig.New()
}

// BuildPriorityAndFairness constructs the guts of the API Priority and Fairness filter
func BuildPriorityAndFairness(s *options.ServerRunOptions, extclient clientgoclientset.Interface, versionedInformer clientgoinformers.SharedInformerFactory) (utilflowcontrol.Interface, error) {
	if s.GenericServerRunOptions.MaxRequestsInFlight+s.GenericServerRunOptions.MaxMutatingRequestsInFlight <= 0 {
		return nil, fmt.Errorf("invalid configuration: MaxRequestsInFlight=%d and MaxMutatingRequestsInFlight=%d; they must add up to something positive", s.GenericServerRunOptions.MaxRequestsInFlight, s.GenericServerRunOptions.MaxMutatingRequestsInFlight)
	}
	return utilflowcontrol.New(
		versionedInformer,
		extclient.FlowcontrolV1beta2(),
		s.GenericServerRunOptions.MaxRequestsInFlight+s.GenericServerRunOptions.MaxMutatingRequestsInFlight,
		s.GenericServerRunOptions.RequestTimeout/4,
	), nil
}

// completedServerRunOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedServerRunOptions struct {
	*options.ServerRunOptions
}

// Complete set default ServerRunOptions.
// Should be called after kube-apiserver flags parsed. 在参数解析后，完成config的填充
func Complete(s *options.ServerRunOptions) (completedServerRunOptions, error) {
	var options completedServerRunOptions
	// set defaults
	if err := s.GenericServerRunOptions.DefaultAdvertiseAddress(s.SecureServing.SecureServingOptions); err != nil {
		return options, err
	}

	// process s.ServiceClusterIPRange from list to Primary and Secondary
	// we process secondary only if provided by user
	apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, err := getServiceIPAndRanges(s.ServiceClusterIPRanges)
	if err != nil {
		return options, err
	}
	s.PrimaryServiceClusterIPRange = primaryServiceIPRange
	s.SecondaryServiceClusterIPRange = secondaryServiceIPRange
	s.APIServerServiceIP = apiServerServiceIP

	if err := s.SecureServing.MaybeDefaultWithSelfSignedCerts(s.GenericServerRunOptions.AdvertiseAddress.String(), []string{"kubernetes.default.svc", "kubernetes.default", "kubernetes"}, []net.IP{apiServerServiceIP}); err != nil {
		return options, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	if len(s.GenericServerRunOptions.ExternalHost) == 0 {
		if len(s.GenericServerRunOptions.AdvertiseAddress) > 0 {
			s.GenericServerRunOptions.ExternalHost = s.GenericServerRunOptions.AdvertiseAddress.String()
		} else {
			if hostname, err := os.Hostname(); err == nil {
				s.GenericServerRunOptions.ExternalHost = hostname
			} else {
				return options, fmt.Errorf("error finding host name: %v", err)
			}
		}
		klog.Infof("external host was not specified, using %v", s.GenericServerRunOptions.ExternalHost)
	}

	s.Authentication.ApplyAuthorization(s.Authorization)

	// Use (ServiceAccountSigningKeyFile != "") as a proxy to the user enabling
	// TokenRequest functionality. This defaulting was convenient, but messed up
	// a lot of people when they rotated their serving cert with no idea it was
	// connected to their service account keys. We are taking this opportunity to
	// remove this problematic defaulting.
	if s.ServiceAccountSigningKeyFile == "" {
		// Default to the private server key for service account token signing
		if len(s.Authentication.ServiceAccounts.KeyFiles) == 0 && s.SecureServing.ServerCert.CertKey.KeyFile != "" {
			if kubeauthenticator.IsValidServiceAccountKeyFile(s.SecureServing.ServerCert.CertKey.KeyFile) {
				s.Authentication.ServiceAccounts.KeyFiles = []string{s.SecureServing.ServerCert.CertKey.KeyFile}
			} else {
				klog.Warning("No TLS key provided, service account token authentication disabled")
			}
		}
	}

	if s.ServiceAccountSigningKeyFile != "" && len(s.Authentication.ServiceAccounts.Issuers) != 0 && s.Authentication.ServiceAccounts.Issuers[0] != "" {
		sk, err := keyutil.PrivateKeyFromFile(s.ServiceAccountSigningKeyFile)
		if err != nil {
			return options, fmt.Errorf("failed to parse service-account-issuer-key-file: %v", err)
		}
		if s.Authentication.ServiceAccounts.MaxExpiration != 0 {
			lowBound := time.Hour
			upBound := time.Duration(1<<32) * time.Second
			if s.Authentication.ServiceAccounts.MaxExpiration < lowBound ||
				s.Authentication.ServiceAccounts.MaxExpiration > upBound {
				return options, fmt.Errorf("the service-account-max-token-expiration must be between 1 hour and 2^32 seconds")
			}
			if s.Authentication.ServiceAccounts.ExtendExpiration {
				if s.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.WarnOnlyBoundTokenExpirationSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, in order to correctly trigger safe transition logic, service-account-max-token-expiration must be set longer than %d seconds (currently %s)", serviceaccount.WarnOnlyBoundTokenExpirationSeconds, s.Authentication.ServiceAccounts.MaxExpiration)
				}
				if s.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.ExpirationExtensionSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, enabling tokens valid up to %d seconds, which is longer than service-account-max-token-expiration set to %s seconds", serviceaccount.ExpirationExtensionSeconds, s.Authentication.ServiceAccounts.MaxExpiration)
				}
			}
		}

		s.ServiceAccountIssuer, err = serviceaccount.JWTTokenGenerator(s.Authentication.ServiceAccounts.Issuers[0], sk)
		if err != nil {
			return options, fmt.Errorf("failed to build token generator: %v", err)
		}
		s.ServiceAccountTokenMaxExpiration = s.Authentication.ServiceAccounts.MaxExpiration
	}

	if s.Etcd.EnableWatchCache {
		sizes := kubeapiserver.DefaultWatchCacheSizes()
		// Ensure that overrides parse correctly.
		userSpecified, err := serveroptions.ParseWatchCacheSizes(s.Etcd.WatchCacheSizes)
		if err != nil {
			return options, err
		}
		for resource, size := range userSpecified {
			sizes[resource] = size
		}
		s.Etcd.WatchCacheSizes, err = serveroptions.WriteWatchCacheSizes(sizes)
		if err != nil {
			return options, err
		}
	}

	for key, value := range s.APIEnablement.RuntimeConfig {
		if key == "v1" || strings.HasPrefix(key, "v1/") ||
			key == "api/v1" || strings.HasPrefix(key, "api/v1/") {
			delete(s.APIEnablement.RuntimeConfig, key)
			s.APIEnablement.RuntimeConfig["/v1"] = value
		}
		if key == "api/legacy" {
			delete(s.APIEnablement.RuntimeConfig, key)
		}
	}

	options.ServerRunOptions = s
	return options, nil
}

func buildServiceResolver(enabledAggregatorRouting bool, hostname string, informer clientgoinformers.SharedInformerFactory) webhook.ServiceResolver {
	var serviceResolver webhook.ServiceResolver
	if enabledAggregatorRouting {
		serviceResolver = aggregatorapiserver.NewEndpointServiceResolver(
			informer.Core().V1().Services().Lister(),
			informer.Core().V1().Endpoints().Lister(),
		)
	} else {
		serviceResolver = aggregatorapiserver.NewClusterIPServiceResolver(
			informer.Core().V1().Services().Lister(),
		)
	}
	// resolve kubernetes.default.svc locally
	if localHost, err := url.Parse(hostname); err == nil {
		serviceResolver = aggregatorapiserver.NewLoopbackServiceResolver(serviceResolver, localHost)
	}
	return serviceResolver
}

func getServiceIPAndRanges(serviceClusterIPRanges string) (net.IP, net.IPNet, net.IPNet, error) {
	serviceClusterIPRangeList := []string{}
	if serviceClusterIPRanges != "" {
		serviceClusterIPRangeList = strings.Split(serviceClusterIPRanges, ",")
	}

	var apiServerServiceIP net.IP
	var primaryServiceIPRange net.IPNet
	var secondaryServiceIPRange net.IPNet
	var err error
	// nothing provided by user, use default range (only applies to the Primary)
	if len(serviceClusterIPRangeList) == 0 {
		var primaryServiceClusterCIDR net.IPNet
		primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(primaryServiceClusterCIDR)
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges: %v", err)
		}
		return apiServerServiceIP, primaryServiceIPRange, net.IPNet{}, nil
	}

	_, primaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[0])
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[0] is not a valid cidr")
	}

	primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(*primaryServiceClusterCIDR)
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges for primary service cidr: %v", err)
	}

	// user provided at least two entries
	// note: validation asserts that the list is max of two dual stack entries
	if len(serviceClusterIPRangeList) > 1 {
		_, secondaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[1])
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[1] is not an ip net")
		}
		secondaryServiceIPRange = *secondaryServiceClusterCIDR
	}
	return apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, nil
}
