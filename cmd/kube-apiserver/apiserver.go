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

// apiserver is the main api server and master for the cluster.
// it is responsible for serving the cluster management API.
package main

import (
	"os"

	"k8s.io/component-base/cli"
	_ "k8s.io/component-base/logs/json/register"          // for JSON log format registration
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // load all the prometheus client-go plugins
	_ "k8s.io/component-base/metrics/prometheus/version"  // for version metric registration
	"k8s.io/kubernetes/cmd/kube-apiserver/app"
)

// kube-apiserver包含三个APIServer：
// - aggregatorServer：暴露的功能类似于一个七层负载均衡，将来自用户的请求拦截转发给其他服务器
// - kubeAPIServer：负责对请求的一些通用处理，包括：认证、鉴权以及各个内建资源的 REST 服务等
// - apiExtensionsServer：主要处理 CustomResourceDefinition（CRD）和 CustomResource（CR）的 REST 请求，也是 Delegation 的最后一环，如果对应 CR 不能被处理的话则会返回 404
// [AggregatorServer 和 APIExtensionsServer 对应两种主要扩展 APIServer 资源的方式，也即分别是 AA 和 CRD]
func main() {
	command := app.NewAPIServerCommand()
	code := cli.Run(command)
	os.Exit(code)
}
