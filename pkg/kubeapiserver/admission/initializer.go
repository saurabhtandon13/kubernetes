/*
Copyright 2016 The Kubernetes Authors.

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

package admission

import (
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset"
	informers "k8s.io/kubernetes/pkg/client/informers/informers_generated/internalversion"
)

// TODO add a `WantsToRun` which takes a stopCh.  Might make it generic.

// WantsInformerFactory defines a function which sets InformerFactory for admission plugins that need it
type WantsInternalClientSet interface {
	SetInternalClientSet(internalclientset.Interface)
	admission.Validator
}

// WantsInformerFactory defines a function which sets InformerFactory for admission plugins that need it
type WantsInformerFactory interface {
	SetInformerFactory(informers.SharedInformerFactory)
	admission.Validator
}

// WantsAuthorizer defines a function which sets Authorizer for admission plugins that need it.
type WantsAuthorizer interface {
	SetAuthorizer(authorizer.Authorizer)
	admission.Validator
}

type pluginInitializer struct {
	internalClient internalclientset.Interface
	informers      informers.SharedInformerFactory
	authorizer     authorizer.Authorizer
}

var _ admission.PluginInitializer = pluginInitializer{}

// NewPluginInitializer constructs new instance of PluginInitializer
func NewPluginInitializer(internalClient internalclientset.Interface, sharedInformers informers.SharedInformerFactory, authz authorizer.Authorizer) admission.PluginInitializer {
	return pluginInitializer{
		internalClient: internalClient,
		informers:      sharedInformers,
		authorizer:     authz,
	}
}

// Initialize checks the initialization interfaces implemented by each plugin
// and provide the appropriate initialization data
func (i pluginInitializer) Initialize(plugin admission.Interface) {
	if wants, ok := plugin.(WantsInternalClientSet); ok {
		wants.SetInternalClientSet(i.internalClient)
	}

	if wants, ok := plugin.(WantsInformerFactory); ok {
		wants.SetInformerFactory(i.informers)
	}

	if wants, ok := plugin.(WantsAuthorizer); ok {
		wants.SetAuthorizer(i.authorizer)
	}
}
