// Copyright Istio Authors
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

package endpoints

import (
	"math"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/core/v1alpha3/loadbalancer"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/kind"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/spiffe"
	"istio.io/istio/pkg/util/hash"
)

var (
	Separator = []byte{'~'}
	Slash     = []byte{'/'}

	// same as the above "xds" package
	log = istiolog.RegisterScope("ads", "ads debugging")
)

// ConnectOriginate is the name for the resources associated with the origination of HTTP CONNECT.
// Duplicated from v1alpha3/waypoint.go to avoid import cycle
const connectOriginate = "connect_originate"

type EndpointBuilder struct {
	// These fields define the primary key for an endpoint, and can be used as a cache key
	clusterName            string
	network                network.ID
	proxyView              model.ProxyView
	clusterID              cluster.ID
	locality               *corev3.Locality
	destinationRule        *model.ConsolidatedDestRule
	service                *model.Service
	clusterLocal           bool
	nodeType               model.NodeType
	failoverPriorityLabels []byte

	// These fields are provided for convenience only
	subsetName   string
	subsetLabels labels.Instance
	hostname     host.Name
	port         int
	push         *model.PushContext
	proxy        *model.Proxy
	dir          model.TrafficDirection

	mtlsChecker *mtlsChecker
}

func NewEndpointBuilder(clusterName string, proxy *model.Proxy, push *model.PushContext) EndpointBuilder {
	dir, subsetName, hostname, port := model.ParseSubsetKey(clusterName)

	svc := push.ServiceForHostname(proxy, hostname)
	var dr *model.ConsolidatedDestRule
	if svc != nil {
		dr = proxy.SidecarScope.DestinationRule(model.TrafficDirectionOutbound, proxy, svc.Hostname)
	}

	return *NewCDSEndpointBuilder(
		proxy, push, clusterName,
		dir, subsetName, hostname, port,
		svc, dr,
	)
}

// NewCDSEndpointBuilder allows setting some fields directly when we already
// have the Service and DestinationRule.
func NewCDSEndpointBuilder(
	proxy *model.Proxy, push *model.PushContext, clusterName string,
	dir model.TrafficDirection, subsetName string, hostname host.Name, port int,
	service *model.Service, dr *model.ConsolidatedDestRule,
) *EndpointBuilder {
	b := EndpointBuilder{
		clusterName:     clusterName,
		network:         proxy.Metadata.Network,
		proxyView:       proxy.GetView(),
		clusterID:       proxy.Metadata.ClusterID,
		locality:        proxy.Locality,
		destinationRule: dr,
		service:         service,
		clusterLocal:    push.IsClusterLocal(service),
		nodeType:        proxy.Type,

		subsetName: subsetName,
		hostname:   hostname,
		port:       port,
		push:       push,
		proxy:      proxy,
		dir:        dir,
	}
	b.populateSubsetInfo()
	b.populateFailoverPriorityLabels()
	return &b
}

func (b *EndpointBuilder) servicePort(port int) *model.Port {
	if !b.ServiceFound() {
		log.Debugf("can not find the service %s for cluster %s", b.hostname, b.clusterName)
		return nil
	}
	svcPort, f := b.service.Ports.GetByPort(port)
	if !f {
		log.Debugf("can not find the service port %d for cluster %s", b.port, b.clusterName)
		return nil
	}
	return svcPort
}

func (b *EndpointBuilder) WithSubset(subset string) *EndpointBuilder {
	if b == nil {
		return nil
	}
	subsetBuilder := *b
	subsetBuilder.subsetName = subset
	subsetBuilder.populateSubsetInfo()
	return &subsetBuilder
}

func (b *EndpointBuilder) populateSubsetInfo() {
	if b.dir == model.TrafficDirectionInboundVIP {
		b.subsetName = strings.TrimPrefix(b.subsetName, "http/")
		b.subsetName = strings.TrimPrefix(b.subsetName, "tcp/")
	}
	b.mtlsChecker = newMtlsChecker(b.push, b.port, b.destinationRule.GetRule(), b.subsetName)
	b.subsetLabels = getSubSetLabels(b.DestinationRule(), b.subsetName)
}

func (b *EndpointBuilder) populateFailoverPriorityLabels() {
	enableFailover, lb := getOutlierDetectionAndLoadBalancerSettings(b.DestinationRule(), b.port, b.subsetName)
	if enableFailover {
		lbSetting := loadbalancer.GetLocalityLbSetting(b.push.Mesh.GetLocalityLbSetting(), lb.GetLocalityLbSetting())
		if lbSetting != nil && lbSetting.Distribute == nil &&
			len(lbSetting.FailoverPriority) > 0 && (lbSetting.Enabled == nil || lbSetting.Enabled.Value) {
			b.failoverPriorityLabels = util.GetFailoverPriorityLabels(b.proxy.Labels, lbSetting.FailoverPriority)
		}
	}
}

func (b *EndpointBuilder) DestinationRule() *v1alpha3.DestinationRule {
	if dr := b.destinationRule.GetRule(); dr != nil {
		dr, _ := dr.Spec.(*v1alpha3.DestinationRule)
		return dr
	}
	return nil
}

func (b *EndpointBuilder) Type() string {
	return model.EDSType
}

func (b *EndpointBuilder) ServiceFound() bool {
	return b.service != nil
}

func (b *EndpointBuilder) IsDNSCluster() bool {
	return b.service != nil && (b.service.Resolution == model.DNSLB || b.service.Resolution == model.DNSRoundRobinLB)
}

// Key provides the eds cache key and should include any information that could change the way endpoints are generated.
func (b *EndpointBuilder) Key() any {
	// nolint: gosec
	// Not security sensitive code
	h := hash.New()
	b.WriteHash(h)
	return h.Sum64()
}

func (b *EndpointBuilder) WriteHash(h hash.Hash) {
	if b == nil {
		return
	}
	h.Write([]byte(b.clusterName))
	h.Write(Separator)
	h.Write([]byte(b.network))
	h.Write(Separator)
	h.Write([]byte(b.clusterID))
	h.Write(Separator)
	h.Write([]byte(b.nodeType))
	h.Write(Separator)
	h.Write([]byte(strconv.FormatBool(b.clusterLocal)))
	h.Write(Separator)
	if features.EnableHBONE && b.proxy != nil {
		h.Write([]byte(strconv.FormatBool(b.proxy.IsProxylessGrpc())))
		h.Write(Separator)
	}
	h.Write([]byte(util.LocalityToString(b.locality)))
	h.Write(Separator)
	if len(b.failoverPriorityLabels) > 0 {
		h.Write(b.failoverPriorityLabels)
		h.Write(Separator)
	}
	if b.service.Attributes.NodeLocal {
		h.Write([]byte(b.proxy.GetNodeName()))
		h.Write(Separator)
	}

	if b.push != nil && b.push.AuthnPolicies != nil {
		h.Write([]byte(b.push.AuthnPolicies.GetVersion()))
	}
	h.Write(Separator)

	for _, dr := range b.destinationRule.GetFrom() {
		h.Write([]byte(dr.Name))
		h.Write(Slash)
		h.Write([]byte(dr.Namespace))
	}
	h.Write(Separator)

	if b.service != nil {
		h.Write([]byte(b.service.Hostname))
		h.Write(Slash)
		h.Write([]byte(b.service.Attributes.Namespace))
	}
	h.Write(Separator)

	if b.proxyView != nil {
		h.Write([]byte(b.proxyView.String()))
	}
	h.Write(Separator)
}

func (b *EndpointBuilder) Cacheable() bool {
	// If service is not defined, we cannot do any caching as we will not have a way to
	// invalidate the results.
	// Service being nil means the EDS will be empty anyways, so not much lost here.
	return b.service != nil
}

func (b *EndpointBuilder) DependentConfigs() []model.ConfigHash {
	drs := b.destinationRule.GetFrom()
	configs := make([]model.ConfigHash, 0, len(drs)+1)
	if b.destinationRule != nil {
		for _, dr := range drs {
			configs = append(configs, model.ConfigKey{
				Kind: kind.DestinationRule,
				Name: dr.Name, Namespace: dr.Namespace,
			}.HashCode())
		}
	}
	if b.service != nil {
		configs = append(configs, model.ConfigKey{
			Kind: kind.ServiceEntry,
			Name: string(b.service.Hostname), Namespace: b.service.Attributes.Namespace,
		}.HashCode())
	}

	// For now, this matches clusterCache's DependentConfigs. If adding anything here, we may need to add them there.

	return configs
}

type LocalityEndpoints struct {
	istioEndpoints []*model.IstioEndpoint
	// The protobuf message which contains LbEndpoint slice.
	llbEndpoints endpoint.LocalityLbEndpoints
}

func (e *LocalityEndpoints) append(ep *model.IstioEndpoint, le *endpoint.LbEndpoint) {
	e.istioEndpoints = append(e.istioEndpoints, ep)
	e.llbEndpoints.LbEndpoints = append(e.llbEndpoints.LbEndpoints, le)
}

func (e *LocalityEndpoints) refreshWeight() {
	var weight *wrapperspb.UInt32Value
	if len(e.llbEndpoints.LbEndpoints) == 0 {
		weight = nil
	} else {
		weight = &wrapperspb.UInt32Value{}
		for _, lbEp := range e.llbEndpoints.LbEndpoints {
			weight.Value += lbEp.GetLoadBalancingWeight().Value
		}
	}
	e.llbEndpoints.LoadBalancingWeight = weight
}

func (e *LocalityEndpoints) AssertInvarianceInTest() {
	if len(e.llbEndpoints.LbEndpoints) != len(e.istioEndpoints) {
		panic(" len(e.llbEndpoints.LbEndpoints) != len(e.tunnelMetadata)")
	}
}

// FromServiceEndpoints builds LocalityLbEndpoints from the PushContext's snapshotted ServiceIndex.
// Used for CDS (ClusterLoadAssignment constructed elsewhere).
func (b *EndpointBuilder) FromServiceEndpoints() []*endpoint.LocalityLbEndpoints {
	if b == nil {
		return nil
	}
	svcEps := b.push.ServiceEndpointsByPort(b.service, b.port, b.subsetLabels)
	// don't use the pre-computed endpoints for CDS to preserve previous behavior
	return ExtractEnvoyEndpoints(b.generate(svcEps, true))
}

// BuildClusterLoadAssignment converts the shards for this EndpointBuilder's Service
// into a ClusterLoadAssignment. Used for EDS.
func (b *EndpointBuilder) BuildClusterLoadAssignment(endpointIndex *model.EndpointIndex) *endpoint.ClusterLoadAssignment {
	svcEps := b.snapshotShards(endpointIndex)
	localityLbEndpoints := b.generate(svcEps, false)
	if len(localityLbEndpoints) == 0 {
		return buildEmptyClusterLoadAssignment(b.clusterName)
	}

	l := b.createClusterLoadAssignment(localityLbEndpoints)

	// If locality aware routing is enabled, prioritize endpoints or set their lb weight.
	// Failover should only be enabled when there is an outlier detection, otherwise Envoy
	// will never detect the hosts are unhealthy and redirect traffic.
	enableFailover, lb := getOutlierDetectionAndLoadBalancerSettings(b.DestinationRule(), b.port, b.subsetName)
	lbSetting := loadbalancer.GetLocalityLbSetting(b.push.Mesh.GetLocalityLbSetting(), lb.GetLocalityLbSetting())
	if lbSetting != nil {
		// Make a shallow copy of the cla as we are mutating the endpoints with priorities/weights relative to the calling proxy
		l = util.CloneClusterLoadAssignment(l)
		wrappedLocalityLbEndpoints := make([]*loadbalancer.WrappedLocalityLbEndpoints, len(localityLbEndpoints))
		for i := range localityLbEndpoints {
			wrappedLocalityLbEndpoints[i] = &loadbalancer.WrappedLocalityLbEndpoints{
				IstioEndpoints:      localityLbEndpoints[i].istioEndpoints,
				LocalityLbEndpoints: l.Endpoints[i],
			}
		}
		loadbalancer.ApplyLocalityLBSetting(l, wrappedLocalityLbEndpoints, b.locality, b.proxy.Labels, lbSetting, enableFailover)
	}
	return l
}

// generate endpoints with applies weights, multi-network mapping and other filtering
// noCache means we will not use or update the IstioEndpoint's precomputedEnvoyEndpoint
func (b *EndpointBuilder) generate(eps []*model.IstioEndpoint, allowPrecomputed bool) []*LocalityEndpoints {
	// shouldn't happen here
	if !b.ServiceFound() {
		return nil
	}
	svcPort := b.servicePort(b.port)
	if svcPort == nil {
		return nil
	}

	eps = slices.Filter(eps, func(ep *model.IstioEndpoint) bool {
		return b.filterIstioEndpoint(ep, svcPort)
	})

	localityEpMap := make(map[string]*LocalityEndpoints)
	for _, ep := range eps {
		eep := ep.EnvoyEndpoint()
		mtlsEnabled := b.mtlsChecker.checkMtlsEnabled(ep)
		// Determine if we need to build the endpoint. We try to cache it for performance reasons
		needToCompute := eep == nil
		if features.EnableHBONE {
			// Currently the HBONE implementation leads to different endpoint generation depending on if the
			// client proxy supports HBONE or not. This breaks the cache.
			// For now, just disable caching if the global HBONE flag is enabled.
			needToCompute = true
		}
		if eep != nil && mtlsEnabled != isMtlsEnabled(eep) {
			// The mTLS settings may have changed, invalidating the cache endpoint. Rebuild it
			needToCompute = true
		}
		if needToCompute || !allowPrecomputed {
			eep = buildEnvoyLbEndpoint(b, ep, mtlsEnabled)
			if eep == nil {
				continue
			}
			if allowPrecomputed {
				ep.ComputeEnvoyEndpoint(eep)
			}
		}
		locLbEps, found := localityEpMap[ep.Locality.Label]
		if !found {
			locLbEps = &LocalityEndpoints{
				llbEndpoints: endpoint.LocalityLbEndpoints{
					Locality:    util.ConvertLocality(ep.Locality.Label),
					LbEndpoints: make([]*endpoint.LbEndpoint, 0, len(eps)),
				},
			}
			localityEpMap[ep.Locality.Label] = locLbEps
		}
		locLbEps.append(ep, eep)
	}

	locEps := make([]*LocalityEndpoints, 0, len(localityEpMap))
	locs := make([]string, 0, len(localityEpMap))
	for k := range localityEpMap {
		locs = append(locs, k)
	}
	if len(locs) >= 2 {
		sort.Strings(locs)
	}
	for _, locality := range locs {
		locLbEps := localityEpMap[locality]
		var weight uint32
		var overflowStatus bool
		for _, ep := range locLbEps.llbEndpoints.LbEndpoints {
			weight, overflowStatus = addUint32(weight, ep.LoadBalancingWeight.GetValue())
		}
		locLbEps.llbEndpoints.LoadBalancingWeight = &wrapperspb.UInt32Value{
			Value: weight,
		}
		if overflowStatus {
			log.Warnf("Sum of localityLbEndpoints weight is overflow: service:%s, port: %d, locality:%s",
				b.service.Hostname, b.port, locality)
		}
		locEps = append(locEps, locLbEps)
	}

	if len(locEps) == 0 {
		b.push.AddMetric(model.ProxyStatusClusterNoInstances, b.clusterName, "", "")
	}

	// Apply the Split Horizon EDS filter, if applicable.
	locEps = b.EndpointsByNetworkFilter(locEps)

	if model.IsDNSSrvSubsetKey(b.clusterName) {
		// For the SNI-DNAT clusters, we are using AUTO_PASSTHROUGH gateway. AUTO_PASSTHROUGH is intended
		// to passthrough mTLS requests. However, at the gateway we do not actually have any way to tell if the
		// request is a valid mTLS request or not, since its passthrough TLS.
		// To ensure we allow traffic only to mTLS endpoints, we filter out non-mTLS endpoints for these cluster types.
		locEps = b.EndpointsWithMTLSFilter(locEps)
	}

	return locEps
}

// addUint32AvoidOverflow returns sum of two uint32 and status. If sum overflows,
// and returns MaxUint32 and status.
func addUint32(left, right uint32) (uint32, bool) {
	if math.MaxUint32-right < left {
		return math.MaxUint32, true
	}
	return left + right, false
}

func (b *EndpointBuilder) filterIstioEndpoint(ep *model.IstioEndpoint, svcPort *model.Port) bool {
	// for ServiceInternalTrafficPolicy
	if b.service.Attributes.NodeLocal && ep.NodeName != b.proxy.GetNodeName() {
		return false
	}
	// Only send endpoints from the networks in the network view requested by the proxy.
	// The default network view assigned to the Proxy is nil, in that case match any network.
	if !b.proxyView.IsVisible(ep) {
		// Endpoint's network doesn't match the set of networks that the proxy wants to see.
		return false
	}
	// If the downstream service is configured as cluster-local, only include endpoints that
	// reside in the same cluster.
	if b.clusterLocal && (b.clusterID != ep.Locality.ClusterID) {
		return false
	}
	// TODO(nmittler): Consider merging discoverability policy with cluster-local
	if !ep.IsDiscoverableFromProxy(b.proxy) {
		return false
	}
	if svcPort.Name != ep.ServicePortName {
		return false
	}
	// Port labels
	if !b.subsetLabels.SubsetOf(ep.Labels) {
		return false
	}
	// If we don't know the address we must eventually use a gateway address
	if ep.Address == "" && ep.Network == b.network {
		return false
	}
	// Draining endpoints are only sent to 'persistent session' clusters.
	draining := ep.HealthStatus == model.Draining ||
		features.DrainingLabel != "" && ep.Labels[features.DrainingLabel] != ""
	if draining {
		persistentSession := b.service.Attributes.Labels[features.PersistentSessionLabel] != ""
		if !persistentSession {
			return false
		}
	}
	return true
}

// snapshotShards into a local slice to avoid lock contention
func (b *EndpointBuilder) snapshotShards(endpointIndex *model.EndpointIndex) []*model.IstioEndpoint {
	shards := b.findShards(endpointIndex)
	if shards == nil {
		return nil
	}

	// Determine whether or not the target service is considered local to the cluster
	// and should, therefore, not be accessed from outside the cluster.
	isClusterLocal := b.clusterLocal

	var eps []*model.IstioEndpoint
	shards.RLock()
	// Extract shard keys so we can iterate in order. This ensures a stable EDS output.
	keys := shards.Keys()
	// The shards are updated independently, now need to filter and merge for this cluster
	for _, shardKey := range keys {
		if shardKey.Cluster != b.clusterID {
			// If the downstream service is configured as cluster-local, only include endpoints that
			// reside in the same cluster.
			if isClusterLocal || b.service.Attributes.NodeLocal {
				continue
			}
		}
		eps = append(eps, shards.Shards[shardKey]...)
	}
	shards.RUnlock()
	return eps
}

// findShards returns the endpoints for a cluster
func (b *EndpointBuilder) findShards(endpointIndex *model.EndpointIndex) *model.EndpointShards {
	if b.service == nil {
		log.Debugf("can not find the service for cluster %s", b.clusterName)
		return nil
	}

	// Service resolution type might have changed and Cluster may be still in the EDS cluster list of "Connection.Clusters".
	// This can happen if a ServiceEntry's resolution is changed from STATIC to DNS which changes the Envoy cluster type from
	// EDS to STRICT_DNS or LOGICAL_DNS. When pushEds is called before Envoy sends the updated cluster list via Endpoint request which in turn
	// will update "Connection.Clusters", we might accidentally send EDS updates for STRICT_DNS cluster. This check guards
	// against such behavior and returns nil. When the updated cluster warms up in Envoy, it would update with new endpoints
	// automatically.
	// Gateways use EDS for Passthrough cluster. So we should allow Passthrough here.
	if b.IsDNSCluster() {
		log.Infof("cluster %s in eds cluster, but its resolution now is updated to %v, skipping it.", b.clusterName, b.service.Resolution)
		return nil
	}

	epShards, f := endpointIndex.ShardsForService(string(b.hostname), b.service.Attributes.Namespace)
	if !f {
		// Shouldn't happen here
		log.Debugf("can not find the endpointShards for cluster %s", b.clusterName)
		return nil
	}
	return epShards
}

// Create the CLusterLoadAssignment. At this moment the options must have been applied to the locality lb endpoints.
func (b *EndpointBuilder) createClusterLoadAssignment(llbOpts []*LocalityEndpoints) *endpoint.ClusterLoadAssignment {
	llbEndpoints := make([]*endpoint.LocalityLbEndpoints, 0, len(llbOpts))
	for _, l := range llbOpts {
		llbEndpoints = append(llbEndpoints, &l.llbEndpoints)
	}
	return &endpoint.ClusterLoadAssignment{
		ClusterName: b.clusterName,
		Endpoints:   llbEndpoints,
	}
}

// cluster with no endpoints
func buildEmptyClusterLoadAssignment(clusterName string) *endpoint.ClusterLoadAssignment {
	return &endpoint.ClusterLoadAssignment{
		ClusterName: clusterName,
	}
}

func (b *EndpointBuilder) gateways() *model.NetworkGateways {
	if b.IsDNSCluster() {
		return b.push.NetworkManager().Unresolved
	}
	return b.push.NetworkManager().NetworkGateways
}

func ExtractEnvoyEndpoints(locEps []*LocalityEndpoints) []*endpoint.LocalityLbEndpoints {
	var locLbEps []*endpoint.LocalityLbEndpoints
	for _, eps := range locEps {
		locLbEps = append(locLbEps, &eps.llbEndpoints)
	}
	return locLbEps
}

// buildEnvoyLbEndpoint packs the endpoint based on istio info.
func buildEnvoyLbEndpoint(b *EndpointBuilder, e *model.IstioEndpoint, mtlsEnabled bool) *endpoint.LbEndpoint {
	addr := util.BuildAddress(e.Address, e.EndpointPort)
	healthStatus := e.HealthStatus
	if features.DrainingLabel != "" && e.Labels[features.DrainingLabel] != "" {
		healthStatus = model.Draining
	}

	ep := &endpoint.LbEndpoint{
		HealthStatus: corev3.HealthStatus(healthStatus),
		LoadBalancingWeight: &wrapperspb.UInt32Value{
			Value: e.GetLoadBalancingWeight(),
		},
		HostIdentifier: &endpoint.LbEndpoint_Endpoint{
			Endpoint: &endpoint.Endpoint{
				Address: addr,
			},
		},
		Metadata: &corev3.Metadata{},
	}

	// Istio telemetry depends on the metadata value being set for endpoints in the mesh.
	// Istio endpoint level tls transport socket configuration depends on this logic
	// Do not remove
	var meta *model.EndpointMetadata
	if features.CanonicalServiceForMeshExternalServiceEntry && b.service.MeshExternal {
		svcLabels := b.service.Attributes.Labels
		if _, ok := svcLabels[model.IstioCanonicalServiceLabelName]; ok {
			meta = e.MetadataClone()
			if meta.Labels == nil {
				meta.Labels = make(map[string]string)
			}
			meta.Labels[model.IstioCanonicalServiceLabelName] = svcLabels[model.IstioCanonicalServiceLabelName]
			meta.Labels[model.IstioCanonicalServiceRevisionLabelName] = svcLabels[model.IstioCanonicalServiceRevisionLabelName]
		} else {
			meta = e.Metadata()
		}
		meta.Namespace = b.service.Attributes.Namespace
	} else {
		meta = e.Metadata()
	}

	// detect if mTLS is possible for this endpoint, used later during ep filtering
	// this must be done while converting IstioEndpoints because we still have workload labels
	if !mtlsEnabled {
		meta.TLSMode = ""
	}
	util.AppendLbEndpointMetadata(meta, ep.Metadata)

	address, port := e.Address, e.EndpointPort
	tunnelAddress, tunnelPort := address, model.HBoneInboundListenPort

	supportsTunnel := false
	// Other side is a waypoint proxy.
	if al := e.Labels[constants.ManagedGatewayLabel]; al == constants.ManagedGatewayMeshControllerLabel {
		supportsTunnel = true
	}

	// Otherwise has ambient enabled. Note: this is a synthetic label, not existing in the real Pod.
	if b.push.SupportsTunnel(e.Network, e.Address) {
		supportsTunnel = true
	}
	// Otherwise supports tunnel
	// Currently we only support HTTP tunnel, so just check for that. If we support more, we will
	// need to pick the right one based on our support overlap.
	if e.SupportsTunnel(model.TunnelHTTP) {
		supportsTunnel = true
	}
	if b.proxy.IsProxylessGrpc() {
		// Proxyless client cannot handle tunneling, even if the server can
		supportsTunnel = false
	}

	if !b.proxy.EnableHBONE() {
		supportsTunnel = false
	}

	// Setup tunnel information, if needed
	if b.dir == model.TrafficDirectionInboundVIP {
		// This is only used in waypoint proxy
		inScope := waypointInScope(b.proxy, e)
		if !inScope {
			// A waypoint can *partially* select a Service in edge cases. In this case, some % of requests will
			// go through the waypoint, and the rest direct. Since these have already been load balanced across,
			// we want to make sure we only send to workloads behind our waypoint
			return nil
		}
		// For inbound, we only use EDS for the VIP cases. The VIP cluster will point to encap listener.
		if supportsTunnel {
			address := e.Address
			tunnelPort := 15008
			// We will connect to CONNECT origination internal listener, telling it to tunnel to ip:15008,
			// and add some detunnel metadata that had the original port.
			ep.Metadata.FilterMetadata[model.TunnelLabelShortName] = util.BuildTunnelMetadataStruct(address, address, int(e.EndpointPort), tunnelPort)
			ep = util.BuildInternalLbEndpoint(connectOriginate, ep.Metadata)
			ep.LoadBalancingWeight = &wrapperspb.UInt32Value{
				Value: e.GetLoadBalancingWeight(),
			}
		}
	} else if supportsTunnel {
		// Support connecting to server side waypoint proxy, if the destination has one. This is for sidecars and ingress.
		if b.dir == model.TrafficDirectionOutbound && !b.proxy.IsWaypointProxy() && !b.proxy.IsAmbient() {
			workloads := findWaypoints(b.push, e)
			if len(workloads) > 0 {
				// TODO: load balance
				tunnelAddress = workloads[0].String()
			}
		}
		// Setup tunnel metadata so requests will go through the tunnel
		ep.HostIdentifier = &endpoint.LbEndpoint_Endpoint{Endpoint: &endpoint.Endpoint{
			Address: util.BuildInternalAddressWithIdentifier(connectOriginate, net.JoinHostPort(address, strconv.Itoa(int(port)))),
		}}
		ep.Metadata.FilterMetadata[model.TunnelLabelShortName] = util.BuildTunnelMetadataStruct(tunnelAddress, address, int(port), tunnelPort)
		ep.Metadata.FilterMetadata[util.EnvoyTransportSocketMetadataKey] = &structpb.Struct{
			Fields: map[string]*structpb.Value{
				model.TunnelLabelShortName: {Kind: &structpb.Value_StringValue{StringValue: model.TunnelHTTP}},
			},
		}
	}

	return ep
}

// waypointInScope computes whether the endpoint is owned by the waypoint
func waypointInScope(waypoint *model.Proxy, e *model.IstioEndpoint) bool {
	scope := waypoint.WaypointScope()
	if scope.Namespace != e.Namespace {
		return false
	}
	ident, _ := spiffe.ParseIdentity(e.ServiceAccount)
	if scope.ServiceAccount != "" && (scope.ServiceAccount != ident.ServiceAccount) {
		return false
	}
	return true
}

func findWaypoints(push *model.PushContext, e *model.IstioEndpoint) []netip.Addr {
	ident, _ := spiffe.ParseIdentity(e.ServiceAccount)
	ips := push.WaypointsFor(model.WaypointScope{
		Namespace:      e.Namespace,
		ServiceAccount: ident.ServiceAccount,
	})
	return ips
}

func getOutlierDetectionAndLoadBalancerSettings(
	destinationRule *v1alpha3.DestinationRule,
	portNumber int,
	subsetName string,
) (bool, *v1alpha3.LoadBalancerSettings) {
	if destinationRule == nil {
		return false, nil
	}
	outlierDetectionEnabled := false
	var lbSettings *v1alpha3.LoadBalancerSettings

	port := &model.Port{Port: portNumber}
	policy := util.MergeTrafficPolicy(nil, destinationRule.TrafficPolicy, port)

	for _, subset := range destinationRule.Subsets {
		if subset.Name == subsetName {
			policy = util.MergeTrafficPolicy(policy, subset.TrafficPolicy, port)
			break
		}
	}

	if policy != nil {
		lbSettings = policy.LoadBalancer
		if policy.OutlierDetection != nil {
			outlierDetectionEnabled = true
		}
	}

	return outlierDetectionEnabled, lbSettings
}

// getSubSetLabels returns the labels associated with a subset of a given service.
func getSubSetLabels(dr *v1alpha3.DestinationRule, subsetName string) labels.Instance {
	// empty subset
	if subsetName == "" {
		return nil
	}

	if dr == nil {
		return nil
	}

	for _, subset := range dr.Subsets {
		if subset.Name == subsetName {
			if len(subset.Labels) == 0 {
				return nil
			}
			return subset.Labels
		}
	}

	return nil
}
