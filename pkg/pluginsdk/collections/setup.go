package collections

import (
	"context"

	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/util/smallset"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	apisettings "github.com/kgateway-dev/kgateway/v2/api/settings"
	"github.com/kgateway-dev/kgateway/v2/pkg/kgateway/wellknown"
	"github.com/kgateway-dev/kgateway/v2/pkg/krtcollections"
	kmetrics "github.com/kgateway-dev/kgateway/v2/pkg/krtcollections/metrics"
	"github.com/kgateway-dev/kgateway/v2/pkg/metrics"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/ir"
	"github.com/kgateway-dev/kgateway/v2/pkg/pluginsdk/krtutil"
)

func (c *CommonCollections) InitCollections(
	ctx context.Context,
	controllerNames smallset.Set[string],
	plugins pluginsdk.Plugin,
	globalSettings apisettings.Settings,
) (*krtcollections.GatewayIndex, *krtcollections.RoutesIndex, *krtcollections.BackendIndex, krt.Collection[ir.EndpointsForBackend]) {
	// discovery filter
	filter := kclient.Filter{ObjectFilter: c.Client.ObjectFilter()}

	//nolint:forbidigo // ObjectFilter is not needed for this client as it is cluster scoped
	gatewayClasses := krt.WrapClient(kclient.New[*gwv1.GatewayClass](c.Client), c.KrtOpts.ToOptions("KubeGatewayClasses")...)

	namespaces, _ := krtcollections.NewNamespaceCollection(ctx, c.Client, c.KrtOpts)

	kubeRawGateways := krt.WrapClient(kclient.NewFilteredDelayed[*gwv1.Gateway](c.Client, wellknown.GatewayGVR, filter), c.KrtOpts.ToOptions("KubeGateways")...)
	metrics.RegisterEvents(kubeRawGateways, kmetrics.GetResourceMetricEventHandler[*gwv1.Gateway]())

	var kubeRawListenerSets krt.Collection[*gwv1.ListenerSet]
	kubeRawListenerSets = krt.WrapClient(kclient.NewDelayedInformer[*gwv1.ListenerSet](c.Client, wellknown.ListenerSetGVR, kubetypes.StandardInformer, filter), c.KrtOpts.ToOptions("KubeListenerSets")...)
	metrics.RegisterEvents(kubeRawListenerSets, kmetrics.GetResourceMetricEventHandler[*gwv1.ListenerSet]())

	var policies *krtcollections.PolicyIndex
	if globalSettings.EnableEnvoy {
		policies = krtcollections.NewPolicyIndex(c.KrtOpts, plugins.ContributesPolicies, globalSettings)
		for _, plugin := range plugins.ContributesPolicies {
			if plugin.Policies != nil {
				metrics.RegisterEvents(plugin.Policies, kmetrics.GetResourceMetricEventHandler[ir.PolicyWrapper]())
			}
		}
	}

	gateways := krtcollections.NewGatewayIndex(krtcollections.GatewayIndexConfig{
		KrtOpts:             c.KrtOpts,
		ControllerNames:     controllerNames,
		EnvoyControllerName: c.ControllerName,
		PolicyIndex:         policies,
		Gateways:            kubeRawGateways,
		ListenerSets:        kubeRawListenerSets,
		GatewayClasses:      gatewayClasses,
		Namespaces:          namespaces,
	},
		krtcollections.WithGatewayForDeployerTransformationFunc(c.options.gatewayForDeployerTransformationFunc),
		krtcollections.WithGatewayForEnvoyTransformationFunc(c.options.gatewayForEnvoyTransformationFunc),
	)

	if !globalSettings.EnableEnvoy {
		// When Envoy is disabled, only the gateway index is needed for the deployer
		return gateways, nil, nil, nil
	}

	// create the KRT clients, remember to also register any needed types in the type registration setup.
	httpRoutes := krt.WrapClient(kclient.NewFilteredDelayed[*gwv1.HTTPRoute](c.Client, wellknown.HTTPRouteGVR, filter), c.KrtOpts.ToOptions("HTTPRoute")...)
	metrics.RegisterEvents(httpRoutes, kmetrics.GetResourceMetricEventHandler[*gwv1.HTTPRoute]())

	var tcproutes krt.Collection[*gwv1a2.TCPRoute]
	var tlsRoutes krt.Collection[*gwv1.TLSRoute]
	tcproutes = krt.WrapClient(kclient.NewDelayedInformer[*gwv1a2.TCPRoute](c.Client, gvr.TCPRoute, kubetypes.StandardInformer, filter), c.KrtOpts.ToOptions("TCPRoute")...)
	tlsRoutes = krt.WrapClient(kclient.NewDelayedInformer[*gwv1.TLSRoute](c.Client, gvr.TLSRoute, kubetypes.StandardInformer, filter), c.KrtOpts.ToOptions("TLSRoute")...)
	metrics.RegisterEvents(tcproutes, kmetrics.GetResourceMetricEventHandler[*gwv1a2.TCPRoute]())
	metrics.RegisterEvents(tlsRoutes, kmetrics.GetResourceMetricEventHandler[*gwv1.TLSRoute]())

	grpcRoutes := krt.WrapClient(kclient.NewFilteredDelayed[*gwv1.GRPCRoute](c.Client, wellknown.GRPCRouteGVR, filter), c.KrtOpts.ToOptions("GRPCRoute")...)
	metrics.RegisterEvents(grpcRoutes, kmetrics.GetResourceMetricEventHandler[*gwv1.GRPCRoute]())

	backendIndex := krtcollections.NewBackendIndex(c.KrtOpts, policies, c.RefGrants)
	initBackends(plugins, backendIndex)
	endpointIRs := initEndpoints(plugins, c.KrtOpts)

	routes := krtcollections.NewRoutesIndex(c.KrtOpts, c.ControllerName, httpRoutes, grpcRoutes, tcproutes, tlsRoutes, policies, backendIndex, c.RefGrants, globalSettings)
	return gateways, routes, backendIndex, endpointIRs
}

func initBackends(plugins pluginsdk.Plugin, backendIndex *krtcollections.BackendIndex) {
	for gk, plugin := range plugins.ContributesBackends {
		if plugin.Backends != nil {
			backendIndex.AddBackends(gk, plugin.Backends, plugin.AliasKinds...)
		}
	}
}

func initEndpoints(plugins pluginsdk.Plugin, krtopts krtutil.KrtOptions) krt.Collection[ir.EndpointsForBackend] {
	allEndpoints := []krt.Collection[ir.EndpointsForBackend]{}
	for _, plugin := range plugins.ContributesBackends {
		if plugin.Endpoints != nil {
			allEndpoints = append(allEndpoints, plugin.Endpoints)
		}
	}
	// build Endpoint intermediate representation from kubernetes service and extensions
	// TODO move kube service to be an extension
	endpointIRs := krt.JoinCollection(allEndpoints, krtopts.ToOptions("EndpointIRs")...)
	return endpointIRs
}
