/*
Copyright 2017 The Kubernetes Authors.

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
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	apiextensionsoptions "k8s.io/apiextensions-apiserver/pkg/cmd/server/options"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/feature"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
	kubeexternalinformers "k8s.io/client-go/informers"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
)

// 创建kubeapi-extension-server配置
func createAPIExtensionsConfig(
	kubeAPIServerConfig genericapiserver.Config,
	externalInformers kubeexternalinformers.SharedInformerFactory,
	pluginInitializers []admission.PluginInitializer,
	commandOptions *options.ServerRunOptions,
	masterCount int,
	serviceResolver webhook.ServiceResolver,
	authResolverWrapper webhook.AuthenticationInfoResolverWrapper,
) (*apiextensionsapiserver.Config, error) {
	// make a shallow copy to let us twiddle a few things
	// most of the config actually remains the same.  We only need to mess with a couple items related to the particulars of the apiextensions
	genericConfig := kubeAPIServerConfig
	genericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}
	genericConfig.RESTOptionsGetter = nil

	// override genericConfig.AdmissionControl with apiextensions' scheme,
	// because apiextensions apiserver should use its own scheme to convert resources.
	err := commandOptions.Admission.ApplyTo(
		&genericConfig,
		externalInformers,
		genericConfig.LoopbackClientConfig,
		feature.DefaultFeatureGate,
		pluginInitializers...)
	if err != nil {
		return nil, err
	}

	// copy the etcd options so we don't mutate originals.
	etcdOptions := *commandOptions.Etcd
	etcdOptions.StorageConfig.Paging = utilfeature.DefaultFeatureGate.Enabled(features.APIListChunking)
	// this is where the true decodable levels come from.
	etcdOptions.StorageConfig.Codec = apiextensionsapiserver.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion, v1.SchemeGroupVersion)
	// prefer the more compact serialization (v1beta1) for storage until http://issue.k8s.io/82292 is resolved for objects whose v1 serialization is too big but whose v1beta1 serialization can be stored
	etcdOptions.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(v1beta1.SchemeGroupVersion, schema.GroupKind{Group: v1beta1.GroupName})
	genericConfig.RESTOptionsGetter = &genericoptions.SimpleRestOptionsFactory{Options: etcdOptions}

	// override MergedResourceConfig with apiextensions defaults and registry
	if err := commandOptions.APIEnablement.ApplyTo(
		&genericConfig,
		apiextensionsapiserver.DefaultAPIResourceConfigSource(),
		apiextensionsapiserver.Scheme); err != nil {
		return nil, err
	}

	apiextensionsConfig := &apiextensionsapiserver.Config{
		GenericConfig: &genericapiserver.RecommendedConfig{
			Config:                genericConfig,
			SharedInformerFactory: externalInformers,
		},
		ExtraConfig: apiextensionsapiserver.ExtraConfig{
			CRDRESTOptionsGetter: apiextensionsoptions.NewCRDRESTOptionsGetter(etcdOptions),
			MasterCount:          masterCount,
			AuthResolverWrapper:  authResolverWrapper,
			ServiceResolver:      serviceResolver,
		},
	}

	// we need to clear the poststarthooks so we don't add them multiple times to all the servers (that fails)
	apiextensionsConfig.GenericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}

	return apiextensionsConfig, nil
}


// APIExtensionsServer 是最先被初始化的，在 createAPIExtensionsServer 中调用 apiextensionsConfig.Complete().New 来完成 server 的初始化，其主要逻辑为：

// 1、首先调用 c.GenericConfig.New 按照go-restful的模式初始化 Container，
//    在 c.GenericConfig.New 中会调用 NewAPIServerHandler 初始化 handler，
//    APIServerHandler 包含了 API Server 使用的多种http.Handler 类型，包括 go-restful 以及 non-go-restful，
//    以及在以上两者之间选择的 Director 对象，go-restful 用于处理已经注册的 handler，
//    non-go-restful 用来处理不存在的 handler，API URI 处理的选择过程为：
//       FullHandlerChain-> Director ->{GoRestfulContainer， NonGoRestfulMux}。
//       在 c.GenericConfig.New 中还会调用 installAPI来添加包括 /、/debug/*、/metrics、/version 等路由信息。
//       三种 server 在初始化时首先都会调用 c.GenericConfig.New 来初始化一个 genericServer，然后进行 API 的注册；
//
// 2、调用 s.GenericAPIServer.InstallAPIGroup 在路由中注册 API Resources，此方法的调用链非常深，
//    主要是为了将需要暴露的 API Resource 注册到 server 中，以便能通过 http 接口进行 resource 的 REST 操作，
//    其他几种 server 在初始化时也都会执行对应的 InstallAPI；
//
// 3、初始化 server 中需要使用的 controller，主要有 openapiController、crdController、namingController、
//    establishingController、nonStructuralSchemaController、apiApprovalController、finalizingController；
//
// 4、将需要启动的 controller 以及 informer 添加到 PostStartHook 中；
func createAPIExtensionsServer(apiextensionsConfig *apiextensionsapiserver.Config, delegateAPIServer genericapiserver.DelegationTarget) (*apiextensionsapiserver.CustomResourceDefinitions, error) {
	return apiextensionsConfig.Complete().New(delegateAPIServer)
}
