package kube

import (
	"context"
	"fmt"
	akashv1 "github.com/ovrclk/akash/pkg/apis/akash.network/v1"
	metricsutils "github.com/ovrclk/akash/util/metrics"
	dtypes "github.com/ovrclk/akash/x/deployment/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	netv1 "k8s.io/api/networking/v1"

	"strings"

	"k8s.io/client-go/util/flowcontrol"
	"os"
	"path"
	"strconv"

	kubeErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/watch"

	ctypes "github.com/ovrclk/akash/provider/cluster/types"
	"github.com/ovrclk/akash/provider/cluster/util"

	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/tendermint/tendermint/libs/log"

	"github.com/ovrclk/akash/manifest"
	akashtypes "github.com/ovrclk/akash/pkg/apis/akash.network/v1"
	akashclient "github.com/ovrclk/akash/pkg/client/clientset/versioned"
	"github.com/ovrclk/akash/provider/cluster"
	"github.com/ovrclk/akash/types"
	mtypes "github.com/ovrclk/akash/x/market/types"
	"k8s.io/client-go/tools/pager"

	"k8s.io/apimachinery/pkg/runtime"
	restclient "k8s.io/client-go/rest"

	sdktypes "github.com/cosmos/cosmos-sdk/types"
)

var (
	ErrLeaseNotFound            = errors.New("kube: lease not found")
	ErrNoDeploymentForLease     = errors.New("kube: no deployments for lease")
	ErrInternalError            = errors.New("kube: internal error")
	ErrNoServiceForLease        = errors.New("no service for that lease")
)

var (
	kubeCallsCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "provider_kube_calls",
	}, []string{"action", "result"})
)

// Client interface includes cluster client
type Client interface {
	cluster.Client
}

var _ Client = (*client)(nil)

type client struct {
	kc                kubernetes.Interface
	ac                akashclient.Interface
	metc              metricsclient.Interface
	ns                string
	settings          Settings
	log               log.Logger
	kubeContentConfig *restclient.Config
}

// NewClient returns new Kubernetes Client instance with provided logger, host and ns. Returns error incase of failure
func NewClient(log log.Logger, ns string, settings Settings) (Client, error) {
	if err := validateSettings(settings); err != nil {
		return nil, err
	}
	return newClientWithSettings(log, ns, settings)
}

func newClientWithSettings(log log.Logger, ns string, settings Settings) (Client, error) {
	ctx := context.Background()

	config, err := openKubeConfig(settings.ConfigPath, log)
	if err != nil {
		return nil, errors.Wrap(err, "kube: error building config flags")
	}
	config.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()

	kc, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "kube: error creating kubernetes client")
	}

	mc, err := akashclient.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "kube: error creating manifest client")
	}

	metc, err := metricsclient.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "kube: error creating metrics client")
	}

	if err := prepareEnvironment(ctx, kc, ns); err != nil {
		return nil, errors.Wrap(err, "kube: error preparing environment")
	}

	if _, err := kc.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		return nil, errors.Wrap(err, "kube: error connecting to kubernetes")
	}

	return &client{
		settings:          settings,
		kc:                kc,
		ac:                mc,
		metc:              metc,
		ns:                ns,
		log:               log.With("module", "provider-cluster-kube"),
		kubeContentConfig: config,
	}, nil

}

func openKubeConfig(cfgPath string, log log.Logger) (*rest.Config, error) {
	// If no value is specified, use a default
	if len(cfgPath) == 0 {
		cfgPath = path.Join(homedir.HomeDir(), ".kube", "config")
	}

	if _, err := os.Stat(cfgPath); err == nil {
		log.Info("using kube config file", "path", cfgPath)
		return clientcmd.BuildConfigFromFlags("", cfgPath)
	}

	log.Info("using in cluster kube config")
	return rest.InClusterConfig()
}


func (c *client) GetDeployments(ctx context.Context, dID dtypes.DeploymentID) ([]ctypes.Deployment, error) {
	labelSelectors := &strings.Builder{}
	fmt.Fprintf(labelSelectors, "%s=%d", akashLeaseDSeqLabelName, dID.DSeq)
	fmt.Fprintf(labelSelectors, ",%s=%s", akashLeaseOwnerLabelName, dID.Owner)

	manifests, err := c.ac.AkashV1().Manifests(c.ns).List(ctx, metav1.ListOptions{
		TypeMeta:             metav1.TypeMeta{},
		LabelSelector:        labelSelectors.String(),
		FieldSelector:        "",
		Watch:                false,
		AllowWatchBookmarks:  false,
		ResourceVersion:      "",
		ResourceVersionMatch: "",
		TimeoutSeconds:       nil,
		Limit:                0,
		Continue:             "",
	})

	if err != nil {
		return nil, err
	}

	result := make([]ctypes.Deployment, len(manifests.Items))
	for i, manifest := range manifests.Items {
		result[i], err = manifest.Deployment()
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (c *client) GetManifestGroup(ctx context.Context, lID mtypes.LeaseID) (bool, akashv1.ManifestGroup, error){
	leaseNamespace := lidNS(lID)

	obj, err := c.ac.AkashV1().Manifests(c.ns).Get(ctx, leaseNamespace, metav1.GetOptions{})
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			c.log.Info("CRD manifest not found", "lease-ns", leaseNamespace)
			return false, akashv1.ManifestGroup{}, nil
		}

		return false, akashv1.ManifestGroup{}, err
	}

	return true, obj.Spec.Group, nil
}

func (c *client) Deployments(ctx context.Context) ([]ctypes.Deployment, error) {
	manifests, err := c.ac.AkashV1().Manifests(c.ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	deployments := make([]ctypes.Deployment, 0, len(manifests.Items))
	for _, manifest := range manifests.Items {
		deployment, err := manifest.Deployment()
		if err != nil {
			return deployments, err
		}
		deployments = append(deployments, deployment)
	}

	return deployments, nil
}

func (c *client) Deploy(ctx context.Context, lid mtypes.LeaseID, group *manifest.Group) error {
	if err := applyNS(ctx, c.kc, newNSBuilder(c.settings, lid, group)); err != nil {
		c.log.Error("applying namespace", "err", err, "lease", lid)
		return err
	}

	if err := applyNetPolicies(ctx, c.kc, newNetPolBuilder(c.settings, lid, group)); err != nil {
		c.log.Error("applying namespace network policies", "err", err, "lease", lid)
		return err
	}

	if err := applyManifest(ctx, c.ac, newManifestBuilder(c.log, c.settings, c.ns, lid, group)); err != nil {
		c.log.Error("applying manifest", "err", err, "lease", lid)
		return err
	}

	if err := cleanupStaleResources(ctx, c.kc, lid, group); err != nil {
		c.log.Error("cleaning stale resources", "err", err, "lease", lid)
		return err
	}

	labels := make(map[string]string)
	appendLeaseLabels(lid, labels)

	for svcIdx := range group.Services {
		service := &group.Services[svcIdx]
		if err := applyDeployment(ctx, c.kc, newDeploymentBuilder(c.log, c.settings, lid, group, service)); err != nil {
			c.log.Error("applying deployment", "err", err, "lease", lid, "service", service.Name)
			return err
		}

		if len(service.Expose) == 0 {
			c.log.Debug("no services", "lease", lid, "service", service.Name)
			continue
		}

		serviceBuilderLocal := newServiceBuilder(c.log, c.settings, lid, group, service, false)
		if serviceBuilderLocal.any() {
			if err := applyService(ctx, c.kc, serviceBuilderLocal); err != nil {
				c.log.Error("applying local service", "err", err, "lease", lid, "service", service.Name)
				return err
			}
		}

		serviceBuilderGlobal := newServiceBuilder(c.log, c.settings, lid, group, service, true)
		if serviceBuilderGlobal.any() {
			if err := applyService(ctx, c.kc, serviceBuilderGlobal); err != nil {
				c.log.Error("applying global service", "err", err, "lease", lid, "service", service.Name)
				return err
			}
		}

		/**
		for expIdx := range service.Expose {
			expose := service.Expose[expIdx]
			if !util.ShouldBeIngress(expose) {
				continue
			}
			if err := applyIngress(ctx, c.kc, newIngressBuilder(c.log, c.settings, lid, group, service, &service.Expose[expIdx], holdHostnames)); err != nil {
				c.log.Error("applying ingress", "err", err, "lease", lid, "service", service.Name, "expose", expose)
				return err
			}
		}**/
	}

	return nil
}

func (c *client) TeardownLease(ctx context.Context, lid mtypes.LeaseID) error {
	leaseNamespace := lidNS(lid)
	result := c.kc.CoreV1().Namespaces().Delete(ctx, leaseNamespace, metav1.DeleteOptions{})

	label := metricsutils.SuccessLabel
	if result != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("namespaces-delete", label).Inc()

	return result
}


func (c *client) DeclareHostname(ctx context.Context, lID mtypes.LeaseID, host string, serviceName string, externalPort uint32) error {
	// Label each entry with the standard labels
	labels := make(map[string]string)
	appendLeaseLabels(lID, labels)

	foundEntry, err := c.ac.AkashV1().ProviderHosts(c.ns).Get(ctx, host, metav1.GetOptions{})
	exists := true
	resourceVersion := foundEntry.ObjectMeta.ResourceVersion
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			exists = false
		} else {
			return err
		}
	}

	obj := akashtypes.ProviderHost{
		ObjectMeta: metav1.ObjectMeta{
			Name: host, // Name is always the hostname, to prevent duplicates
			Labels: labels,
			ResourceVersion: resourceVersion,
		},
		Spec:       akashtypes.ProviderHostSpec{
			Hostname:    host,
			Owner:       lID.GetOwner(),
			Dseq:        lID.GetDSeq(),
			Oseq: 		 lID.GetOSeq(),
			Gseq:        lID.GetGSeq(),
			Provider: 	lID.GetProvider(),
			ServiceName: serviceName,
			ExternalPort: externalPort,
		},
		Status:     akashtypes.ProviderHostStatus{},
	}

	c.log.Info("declaring hostname", "lease", lID, "service-name", serviceName, "external-port", externalPort)
	// Create or update the entry
	if exists {
		_, err = c.ac.AkashV1().ProviderHosts(c.ns).Update(ctx, &obj, metav1.UpdateOptions{})
	} else {
		_, err = c.ac.AkashV1().ProviderHosts(c.ns).Create(ctx, &obj, metav1.CreateOptions{})
	}
	return err
}

func kubeSelectorForLease(dst *strings.Builder, lID mtypes.LeaseID) {
	fmt.Fprintf(dst, "%s=%s", akashLeaseOwnerLabelName, lID.Owner)
	fmt.Fprintf(dst, ",%s=%d", akashLeaseDSeqLabelName, lID.DSeq)
	fmt.Fprintf(dst, ",%s=%d", akashLeaseGSeqLabelName, lID.GSeq)
	fmt.Fprintf(dst, ",%s=%d", akashLeaseOSeqLabelName, lID.OSeq)
}

func (c *client) PurgeDeclaredHostnames(ctx context.Context, lID mtypes.LeaseID) error {
	labelSelector := &strings.Builder{}
	kubeSelectorForLease(labelSelector, lID)
	result := c.ac.AkashV1().ProviderHosts(c.ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		LabelSelector:       labelSelector.String(),
	})

	return result
}

func newEventsFeedList(ctx context.Context, events []eventsv1.Event) ctypes.EventsWatcher {
	wtch := ctypes.NewEventsFeed(ctx)

	go func() {
		defer wtch.Shutdown()

	done:
		for _, evt := range events {
			evt := evt
			if !wtch.SendEvent(&evt) {
				break done
			}
		}
	}()

	return wtch
}

func newEventsFeedWatch(ctx context.Context, events watch.Interface) ctypes.EventsWatcher {
	wtch := ctypes.NewEventsFeed(ctx)

	go func() {
		defer func() {
			events.Stop()
			wtch.Shutdown()
		}()

	done:
		for {
			select {
			case obj := <-events.ResultChan():
				evt := obj.Object.(*eventsv1.Event)
				if !wtch.SendEvent(evt) {
					break done
				}
			case <-wtch.Done():
				break done
			}
		}
	}()

	return wtch
}

func (c *client) LeaseEvents(ctx context.Context, lid mtypes.LeaseID, services string, follow bool) (ctypes.EventsWatcher, error) {
	if err := c.leaseExists(ctx, lid); err != nil {
		return nil, err
	}

	listOpts := metav1.ListOptions{}
	if len(services) != 0 {
		listOpts.LabelSelector = fmt.Sprintf(akashManifestServiceLabelName+" in (%s)", services)
	}

	var wtch ctypes.EventsWatcher
	if follow {
		watcher, err := c.kc.EventsV1().Events(lidNS(lid)).Watch(ctx, listOpts)
		label := metricsutils.SuccessLabel
		if err != nil {
			label = metricsutils.FailLabel
		}
		kubeCallsCounter.WithLabelValues("events-follow", label).Inc()
		if err != nil {
			return nil, err
		}

		wtch = newEventsFeedWatch(ctx, watcher)
	} else {
		list, err := c.kc.EventsV1().Events(lidNS(lid)).List(ctx, listOpts)
		label := metricsutils.SuccessLabel
		if err != nil {
			label = metricsutils.FailLabel
		}
		kubeCallsCounter.WithLabelValues("events-list", label).Inc()
		if err != nil {
			return nil, err
		}

		wtch = newEventsFeedList(ctx, list.Items)
	}

	return wtch, nil
}

func (c *client) LeaseLogs(ctx context.Context, lid mtypes.LeaseID,
	services string, follow bool, tailLines *int64) ([]*ctypes.ServiceLog, error) {
	if err := c.leaseExists(ctx, lid); err != nil {
		return nil, err
	}

	listOpts := metav1.ListOptions{}
	if len(services) != 0 {
		listOpts.LabelSelector = fmt.Sprintf(akashManifestServiceLabelName+" in (%s)", services)
	}

	c.log.Error("filtering pods", "labelSelector", listOpts.LabelSelector)

	pods, err := c.kc.CoreV1().Pods(lidNS(lid)).List(ctx, listOpts)
	label := metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("pods-list", label).Inc()
	if err != nil {
		c.log.Error("listing pods", "err", err)
		return nil, errors.Wrap(err, ErrInternalError.Error())
	}
	streams := make([]*ctypes.ServiceLog, len(pods.Items))
	for i, pod := range pods.Items {
		stream, err := c.kc.CoreV1().Pods(lidNS(lid)).GetLogs(pod.Name, &corev1.PodLogOptions{
			Follow:     follow,
			TailLines:  tailLines,
			Timestamps: false,
		}).Stream(ctx)
		label := metricsutils.SuccessLabel
		if err != nil {
			label = metricsutils.FailLabel
		}
		kubeCallsCounter.WithLabelValues("pods-getlogs", label).Inc()
		if err != nil {
			c.log.Error("get pod logs", "err", err)
			return nil, errors.Wrap(err, ErrInternalError.Error())
		}
		streams[i] = cluster.NewServiceLog(pod.Name, stream)
	}
	return streams, nil
}

// todo: limit number of results and do pagination / streaming
func (c *client) LeaseStatus(ctx context.Context, lid mtypes.LeaseID) (*ctypes.LeaseStatus, error) {
	deployments, err := c.deploymentsForLease(ctx, lid)
	if err != nil {
		return nil, err
	}
	labelSelector := &strings.Builder{}
	kubeSelectorForLease(labelSelector, lid)
	// TODO - paginate?
	phResult, err := c.ac.AkashV1().ProviderHosts(c.ns).List(ctx, metav1.ListOptions{
		LabelSelector:        labelSelector.String(),
	})

	if err != nil {
		return nil, err
	}

	serviceStatus := make(map[string]*ctypes.ServiceStatus, len(deployments))
	forwardedPorts := make(map[string][]ctypes.ForwardedPortStatus, len(deployments))
	for _, deployment := range deployments {
		status := &ctypes.ServiceStatus{
			Name:               deployment.Name,
			Available:          deployment.Status.AvailableReplicas,
			Total:              deployment.Status.Replicas,
			ObservedGeneration: deployment.Status.ObservedGeneration,
			Replicas:           deployment.Status.Replicas,
			UpdatedReplicas:    deployment.Status.UpdatedReplicas,
			ReadyReplicas:      deployment.Status.ReadyReplicas,
			AvailableReplicas:  deployment.Status.AvailableReplicas,
		}
		serviceStatus[deployment.Name] = status
	}

	for _, ph := range phResult.Items {
		for _, serviceStatus := range serviceStatus {
			if ph.Spec.ServiceName == serviceStatus.Name {
				serviceStatus.URIs = append(serviceStatus.URIs, ph.Spec.Hostname)
			}
		}
	}

	ingress, err := c.kc.NetworkingV1().Ingresses(lidNS(lid)).List(ctx, metav1.ListOptions{})
	label := metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("ingresses-list", label).Inc()
	if err != nil {
		c.log.Error("list ingresses", "err", err)
		return nil, errors.Wrap(err, ErrInternalError.Error())
	}

	services, err := c.kc.CoreV1().Services(lidNS(lid)).List(ctx, metav1.ListOptions{})
	label = metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("services-list", label).Inc()
	if err != nil {
		c.log.Error("list services", "err", err)
		return nil, errors.Wrap(err, ErrInternalError.Error())
	}

	// TODO - change this not to search for ingress, as they aren't guaranteed to exist
	for _, ing := range ingress.Items {
		service, found := serviceStatus[ing.Name]
		if !found {
			continue
		}
		hosts := make([]string, 0, len(ing.Spec.Rules)+(len(ing.Status.LoadBalancer.Ingress)*2))
		for _, rule := range ing.Spec.Rules {
			hosts = append(hosts, rule.Host)
		}
		if c.settings.DeploymentIngressExposeLBHosts {
			for _, lbing := range ing.Status.LoadBalancer.Ingress {
				if val := lbing.IP; val != "" {
					hosts = append(hosts, val)
				}
				if val := lbing.Hostname; val != "" {
					hosts = append(hosts, val)
				}
			}
		}

		service.URIs = hosts
	}

	// Search for a Kubernetes service declared as nodeport
	for _, service := range services.Items {
		if service.Spec.Type == corev1.ServiceTypeNodePort {
			serviceName := service.Name // Always suffixed during creation, so chop it off
			deploymentName := serviceName[0 : len(serviceName)-len(suffixForNodePortServiceName)]
			deployment, ok := serviceStatus[deploymentName]
			if ok && 0 != len(service.Spec.Ports) {
				portsForDeployment := make([]ctypes.ForwardedPortStatus, 0, len(service.Spec.Ports))
				for _, port := range service.Spec.Ports {
					// Check if the service is exposed via NodePort mechanism in the cluster
					// This is a random port chosen by the cluster when the deployment is created
					nodePort := port.NodePort
					if nodePort > 0 {
						// Record the actual port inside the container that is exposed
						v := ctypes.ForwardedPortStatus{
							Host:         c.exposedHostForPort(),
							Port:         uint16(port.TargetPort.IntVal),
							ExternalPort: uint16(nodePort),
							Available:    deployment.Available,
							Name:         deploymentName,
						}

						isValid := true
						switch port.Protocol {
						case corev1.ProtocolTCP:
							v.Proto = manifest.TCP
						case corev1.ProtocolUDP:
							v.Proto = manifest.UDP
						default:
							isValid = false // Skip this, since the Protocol is set to something not supported by Akash
						}
						if isValid {
							portsForDeployment = append(portsForDeployment, v)
						}
					}
				}
				forwardedPorts[deploymentName] = portsForDeployment
			}
		}
	}

	response := &ctypes.LeaseStatus{
		Services:       serviceStatus,
		ForwardedPorts: forwardedPorts,
	}

	return response, nil
}

func (c *client) exposedHostForPort() string {
	return c.settings.ClusterPublicHostname
}

func (c *client) ServiceStatus(ctx context.Context, lid mtypes.LeaseID, name string) (*ctypes.ServiceStatus, error) {
	if err := c.leaseExists(ctx, lid); err != nil {
		return nil, err
	}

	c.log.Debug("get deployment", "lease-ns", lidNS(lid), "name", name)
	deployment, err := c.kc.AppsV1().Deployments(lidNS(lid)).Get(ctx, name, metav1.GetOptions{})
	label := metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("deployments-get", label).Inc()

	if err != nil {
		c.log.Error("deployment get", "err", err)
		return nil, errors.Wrap(err, ErrInternalError.Error())
	}
	if deployment == nil {
		c.log.Error("no deployment found", "name", name)
		return nil, ErrNoDeploymentForLease
	}

	hasIngress := false
	// Get manifest definition from CRD
	c.log.Debug("Pulling manifest from CRD", "lease-ns", lidNS(lid))
	obj, err := c.ac.AkashV1().Manifests(c.ns).Get(ctx, lidNS(lid), metav1.GetOptions{})
	if err != nil {
		c.log.Error("CRD manifest not found", "lease-ns", lidNS(lid), "name", name)
		return nil, err
	}

	found := false
exposeCheckLoop:
	for _, service := range obj.Spec.Group.Services {
		if service.Name == name {
			found = true
			for _, expose := range service.Expose {

				proto, err := manifest.ParseServiceProtocol(expose.Proto)
				if err != nil {
					return nil, err
				}
				mse := manifest.ServiceExpose{
					Port:         expose.Port,
					ExternalPort: expose.ExternalPort,
					Proto:        proto,
					Service:      expose.Service,
					Global:       expose.Global,
					Hosts:        expose.Hosts,
				}
				if util.ShouldBeIngress(mse) {
					hasIngress = true
					break exposeCheckLoop
				}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: service %q", ErrNoServiceForLease, name)
	}

	c.log.Debug("service result", "lease-ns", lidNS(lid), "hasIngress", hasIngress)

	result := &ctypes.ServiceStatus{
		Name:               deployment.Name,
		Available:          deployment.Status.AvailableReplicas,
		Total:              deployment.Status.Replicas,
		ObservedGeneration: deployment.Status.ObservedGeneration,
		Replicas:           deployment.Status.Replicas,
		UpdatedReplicas:    deployment.Status.UpdatedReplicas,
		ReadyReplicas:      deployment.Status.ReadyReplicas,
		AvailableReplicas:  deployment.Status.AvailableReplicas,
	}

	if hasIngress {
		ingress, err := c.kc.NetworkingV1().Ingresses(lidNS(lid)).Get(ctx, name, metav1.GetOptions{})
		label := metricsutils.SuccessLabel
		if err != nil {
			label = metricsutils.FailLabel
		}
		kubeCallsCounter.WithLabelValues("networking-ingresses", label).Inc()
		if err != nil {
			c.log.Error("ingresses get", "err", err)
			return nil, errors.Wrap(err, ErrInternalError.Error())
		}

		hosts := make([]string, 0, len(ingress.Spec.Rules))
		for _, rule := range ingress.Spec.Rules {
			hosts = append(hosts, rule.Host)
		}

		if c.settings.DeploymentIngressExposeLBHosts {
			for _, lbing := range ingress.Status.LoadBalancer.Ingress {
				if val := lbing.IP; val != "" {
					hosts = append(hosts, val)
				}
				if val := lbing.Hostname; val != "" {
					hosts = append(hosts, val)
				}
			}
		}

		result.URIs = hosts
	}

	return result, nil
}

func (c *client) Inventory(ctx context.Context) ([]ctypes.Node, error) {
	// Load all the nodes
	knodes, err := c.activeNodes(ctx)
	if err != nil {
		return nil, err
	}

	nodes := make([]ctypes.Node, 0, len(knodes))
	// Iterate over the node metrics
	for nodeName, knode := range knodes {

		// Get the amount of available CPU, then subtract that in use
		var tmp resource.Quantity

		tmp = knode.cpu.allocatable
		cpuTotal := (&tmp).MilliValue()

		tmp = knode.memory.allocatable
		memoryTotal := (&tmp).Value()

		tmp = knode.storage.allocatable
		storageTotal := (&tmp).Value()

		tmp = knode.cpu.available()
		cpuAvailable := (&tmp).MilliValue()
		if cpuAvailable < 0 {
			cpuAvailable = 0
		}

		tmp = knode.memory.available()
		memoryAvailable := (&tmp).Value()
		if memoryAvailable < 0 {
			memoryAvailable = 0
		}

		tmp = knode.storage.available()
		storageAvailable := (&tmp).Value()
		if storageAvailable < 0 {
			storageAvailable = 0
		}

		resources := types.ResourceUnits{
			CPU: &types.CPU{
				Units: types.NewResourceValue(uint64(cpuAvailable)),
				Attributes: []types.Attribute{
					{
						Key:   "arch",
						Value: knode.arch,
					},
					// todo (#788) other node attributes ?
				},
			},
			Memory: &types.Memory{
				Quantity: types.NewResourceValue(uint64(memoryAvailable)),
				// todo (#788) memory attributes ?
			},
			Storage: &types.Storage{
				Quantity: types.NewResourceValue(uint64(storageAvailable)),
				// todo (#788) storage attributes like class and iops?
			},
		}

		allocateable := types.ResourceUnits{
			CPU: &types.CPU{
				Units: types.NewResourceValue(uint64(cpuTotal)),
				Attributes: []types.Attribute{
					{
						Key:   "arch",
						Value: knode.arch,
					},
					// todo (#788) other node attributes ?
				},
			},
			Memory: &types.Memory{
				Quantity: types.NewResourceValue(uint64(memoryTotal)),
				// todo (#788) memory attributes ?
			},
			Storage: &types.Storage{
				Quantity: types.NewResourceValue(uint64(storageTotal)),
				// todo (#788) storage attributes like class and iops?
			},
		}

		nodes = append(nodes, cluster.NewNode(nodeName, allocateable, resources))
	}

	return nodes, nil
}

type resourcePair struct {
	allocatable resource.Quantity
	allocated   resource.Quantity
}

func (rp resourcePair) available() resource.Quantity {
	result := rp.allocatable.DeepCopy()
	// Modifies the value in place
	(&result).Sub(rp.allocated)
	return result
}

type nodeResources struct {
	cpu     resourcePair
	memory  resourcePair
	storage resourcePair
	arch    string
}

func (c *client) activeNodes(ctx context.Context) (map[string]nodeResources, error) {
	knodes, err := c.kc.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	label := metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("nodes-list", label).Inc()
	if err != nil {
		return nil, err
	}

	podListOptions := metav1.ListOptions{
		FieldSelector: "status.phase!=Failed,status.phase!=Succeeded",
	}
	podsClient := c.kc.CoreV1().Pods(metav1.NamespaceAll)
	podsPager := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return podsClient.List(ctx, opts)
	})
	zero := resource.NewMilliQuantity(0, "m")

	retnodes := make(map[string]nodeResources)
	for _, knode := range knodes.Items {

		if !c.nodeIsActive(knode) {
			continue
		}

		// Create an entry with the allocatable amount for the node
		cpu := knode.Status.Allocatable.Cpu().DeepCopy()
		memory := knode.Status.Allocatable.Memory().DeepCopy()
		storage := knode.Status.Allocatable.StorageEphemeral().DeepCopy()

		entry := nodeResources{
			arch: knode.Status.NodeInfo.Architecture,
			cpu: resourcePair{
				allocatable: cpu,
			},
			memory: resourcePair{
				allocatable: memory,
			},
			storage: resourcePair{
				allocatable: storage,
			},
		}

		// Initialize the allocated amount to for each node
		zero.DeepCopyInto(&entry.cpu.allocated)
		zero.DeepCopyInto(&entry.memory.allocated)
		zero.DeepCopyInto(&entry.storage.allocated)

		retnodes[knode.Name] = entry
	}

	// Go over each pod and sum the resources for it into the value for the pod it lives on
	err = podsPager.EachListItem(ctx, podListOptions, func(obj runtime.Object) error {
		pod := obj.(*corev1.Pod)
		nodeName := pod.Spec.NodeName

		entry := retnodes[nodeName]
		cpuAllocated := &entry.cpu.allocated
		memoryAllocated := &entry.memory.allocated
		storageAllocated := &entry.storage.allocated
		for _, container := range pod.Spec.Containers {
			// Per the documentation Limits > Requests for each pod. But stuff in the kube-system
			// namespace doesn't follow this. The requests is always summed here since it is what
			// the cluster considers a dedicated resource

			cpuAllocated.Add(*container.Resources.Requests.Cpu())
			memoryAllocated.Add(*container.Resources.Requests.Memory())
			storageAllocated.Add(*container.Resources.Requests.StorageEphemeral())
		}

		retnodes[nodeName] = entry // Map is by value, so store the copy back into the map
		return nil
	})
	if err != nil {
		return nil, err
	}

	return retnodes, nil
}

func (c *client) nodeIsActive(node corev1.Node) bool {
	ready := false
	issues := 0

	for _, cond := range node.Status.Conditions {
		switch cond.Type {

		case corev1.NodeReady:

			if cond.Status == corev1.ConditionTrue {
				ready = true
			}

		case corev1.NodeMemoryPressure:
			fallthrough
		case corev1.NodeDiskPressure:
			fallthrough
		case corev1.NodePIDPressure:
			fallthrough
		case corev1.NodeNetworkUnavailable:

			if cond.Status != corev1.ConditionFalse {

				c.log.Error("node in poor condition",
					"node", node.Name,
					"condition", cond.Type,
					"status", cond.Status)

				issues++
			}
		}
	}

	return ready && issues == 0
}

func (c *client) leaseExists(ctx context.Context, lid mtypes.LeaseID) error {
	_, err := c.kc.CoreV1().Namespaces().Get(ctx, lidNS(lid), metav1.GetOptions{})
	label := metricsutils.SuccessLabel
	if err != nil && !kubeErrors.IsNotFound(err) {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("namespace-get", label).Inc()
	if err != nil {
		if kubeErrors.IsNotFound(err) {
			return ErrLeaseNotFound
		}

		c.log.Error("namespaces get", "err", err)
		return errors.Wrap(err, ErrInternalError.Error())
	}

	return nil
}

func (c *client) deploymentsForLease(ctx context.Context, lid mtypes.LeaseID) ([]appsv1.Deployment, error) {
	if err := c.leaseExists(ctx, lid); err != nil {
		return nil, err
	}

	deployments, err := c.kc.AppsV1().Deployments(lidNS(lid)).List(ctx, metav1.ListOptions{})
	label := metricsutils.SuccessLabel
	if err != nil {
		label = metricsutils.FailLabel
	}
	kubeCallsCounter.WithLabelValues("deployments-list", label).Inc()

	if err != nil {
		c.log.Error("deployments list", "err", err)
		return nil, errors.Wrap(err, ErrInternalError.Error())
	}

	if deployments == nil || 0 == len(deployments.Items) {
		c.log.Info("No deployments found for", "lease namespace", lidNS(lid))
		return nil, ErrNoDeploymentForLease
	}

	return deployments.Items, nil
}

type hostnameResourceEvent struct {
	eventType cluster.ProviderResourceEvent
	hostname string

	owner sdktypes.Address
	dseq uint64
	oseq uint32
	gseq uint32
	provider sdktypes.Address
	serviceName string
	externalPort uint32
}

func (ev hostnameResourceEvent) GetLeaseID() mtypes.LeaseID {
	return mtypes.LeaseID{
		Owner:    ev.owner.String(),
		DSeq:     ev.dseq,
		GSeq:     ev.gseq,
		OSeq:     ev.oseq,
		Provider: ev.provider.String(),
	}
}

func (ev hostnameResourceEvent) GetHostname() string {
	return ev.hostname
}

func (ev hostnameResourceEvent) GetEventType() cluster.ProviderResourceEvent {
	return ev.eventType
}

func (ev hostnameResourceEvent) GetServiceName() string {
	return ev.serviceName
}

func (ev hostnameResourceEvent) GetExternalPort() uint32 {
	return ev.externalPort
}

func (c *client) ObserveHostnameState(ctx context.Context) (<- chan cluster.HostnameResourceEvent, error) {
	var lastResourceVersion string
	phpager := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		resources, err := c.ac.AkashV1().ProviderHosts(c.ns).List(ctx, opts)

		if err == nil && len(resources.GetResourceVersion()) != 0 {
			lastResourceVersion = resources.GetResourceVersion()
		}
		return resources, err
	})

	data := make([]akashtypes.ProviderHost, 0, 128)
	err := phpager.EachListItem(ctx, metav1.ListOptions{}, func(obj runtime.Object) error {
		ph := obj.(*akashtypes.ProviderHost)
		data = append(data, *ph)
		return nil
	})

	if err != nil {
		return nil, err
	}

	c.log.Info("starting hostname watch","resourceVersion", lastResourceVersion)
	watcher, err := c.ac.AkashV1().ProviderHosts(c.ns).Watch(ctx, metav1.ListOptions{
		TypeMeta:             metav1.TypeMeta{},
		LabelSelector:        "",
		FieldSelector:        "",
		Watch:                false,
		AllowWatchBookmarks:  false,
		ResourceVersion:      lastResourceVersion,
		ResourceVersionMatch: "",
		TimeoutSeconds:       nil,
		Limit:                0,
		Continue:             "",
	})
	if err != nil {
		return nil, err
	}

	evData := make([]hostnameResourceEvent, len(data))
	for i, v := range data {
		ownerAddr, err := sdktypes.AccAddressFromBech32(v.Spec.Owner)
		if err != nil {
			return nil, err
		}
		providerAddr, err := sdktypes.AccAddressFromBech32(v.Spec.Provider)
		if err != nil {
			return nil, err
		}
		ev := hostnameResourceEvent{
			eventType: cluster.ProviderResourceAdd,
			hostname:  v.Spec.Hostname,
			oseq: v.Spec.Oseq,
			gseq: v.Spec.Gseq,
			dseq:      v.Spec.Dseq,
			owner:     ownerAddr,
			provider: providerAddr,
			serviceName: v.Spec.ServiceName,
			externalPort: v.Spec.ExternalPort,
		}
		evData[i] = ev
	}

	data = nil

	output := make(chan cluster.HostnameResourceEvent)

	go func () {
		defer close(output)
		for _, v := range evData {
			output <- v
		}
		evData = nil // do not hold the reference

		results := watcher.ResultChan()
		for {
			select {
			case result, ok := <-results:
				if !ok { // Channel closed when an error happens
					return
				}
				ph := result.Object.(*akashtypes.ProviderHost)
				ownerAddr, err := sdktypes.AccAddressFromBech32(ph.Spec.Owner)
				if err != nil {
					// ?
					panic(err)
				}
				providerAddr, err := sdktypes.AccAddressFromBech32(ph.Spec.Provider)
				if err != nil {
					// ?
					panic(err)
				}
				ev := hostnameResourceEvent{
					hostname:  ph.Spec.Hostname,
					dseq:      ph.Spec.Dseq,
					oseq: ph.Spec.Oseq,
					gseq: ph.Spec.Gseq,
					owner:     ownerAddr,
					provider: providerAddr,
					serviceName: ph.Spec.ServiceName,
					externalPort: ph.Spec.ExternalPort,
				}
				switch result.Type {

				case watch.Added:
					ev.eventType = cluster.ProviderResourceAdd
				case watch.Modified:
					ev.eventType = cluster.ProviderResourceUpdate
				case watch.Deleted:
					ev.eventType = cluster.ProviderResourceDelete

				default:
					// TODO - check for watch.Error and get data from that?
					continue
				}

				output <- ev

			case <-ctx.Done():
				return
			}
		}
	}()

	return output, nil
}

func (c *client) ConnectHostnameToDeployment(ctx context.Context, hostname string, leaseID mtypes.LeaseID, serviceName string, servicePort int32) error {
	ingressName := hostname
	ns := lidNS(leaseID)
	rules := ingressRules(hostname, serviceName, servicePort)

	_, err := c.kc.NetworkingV1().Ingresses(ns).Get(ctx, ingressName, metav1.GetOptions{})
	metricsutils.IncCounterVecWithLabelValuesFiltered(kubeCallsCounter, "ingresses-get", err, kubeErrors.IsNotFound)

	labels := make(map[string]string)
	appendLeaseLabels(leaseID, labels)

	obj := &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:   ingressName,
			Labels: labels,
		},
		Spec: netv1.IngressSpec{
			Rules: rules,
		},
	}

	switch {
	case err == nil:
		_, err = c.kc.NetworkingV1().Ingresses(ns).Update(ctx, obj, metav1.UpdateOptions{})
		metricsutils.IncCounterVecWithLabelValues(kubeCallsCounter, "networking-ingresses-update", err)
	case kubeErrors.IsNotFound(err):
		_, err = c.kc.NetworkingV1().Ingresses(ns).Create(ctx, obj, metav1.CreateOptions{})
		metricsutils.IncCounterVecWithLabelValues(kubeCallsCounter, "networking-ingresses-create", err)
	}

	return err
}

func (c *client) RemoveHostnameFromDeployment(ctx context.Context, hostname string, leaseID mtypes.LeaseID, allowMissing bool) error {
	ns := lidNS(leaseID)
	labelSelector := &strings.Builder{}
	kubeSelectorForLease(labelSelector, leaseID)

	fieldSelector := &strings.Builder{}
	fmt.Fprintf(fieldSelector, "metadata.name=%s", hostname)

	// This delete only works if the ingress exists & the labels match the lease ID given
	err := c.kc.NetworkingV1().Ingresses(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{
		TypeMeta:             metav1.TypeMeta{},
		LabelSelector:        labelSelector.String(),
		FieldSelector:        fieldSelector.String(),
		Watch:                false,
		AllowWatchBookmarks:  false,
		ResourceVersion:      "",
		ResourceVersionMatch: "",
		TimeoutSeconds:       nil,
		Limit:                0,
		Continue:             "",
	})

	if err != nil && allowMissing && kubeErrors.IsNotFound(err) {
		return nil
	}

	return err
}

var anError = errors.New("boom")

type leaseIdHostnameConnection struct {
	leaseID mtypes.LeaseID
	hostname string
	externalPort int32
	serviceName string
}

func (lh leaseIdHostnameConnection) GetHostname() string {
	return lh.hostname
}

func (lh leaseIdHostnameConnection) GetLeaseID() mtypes.LeaseID {
	return lh.leaseID
}

func (lh leaseIdHostnameConnection) GetExternalPort() int32 {
	return lh.externalPort
}

func (lh leaseIdHostnameConnection) GetServiceName() string {
	return lh.serviceName
}

func (c *client) GetHostnameDeploymentConnections(ctx context.Context) ([]cluster.LeaseIdHostnameConnection, error) {
	ingressPager := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return c.kc.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, opts)
	})

	results := make([]cluster.LeaseIdHostnameConnection, 0)
	err := ingressPager.EachListItem(ctx, metav1.ListOptions{ /*TODO - filter to labeled */}, func(obj runtime.Object) error {
		ingress := obj.(*netv1.Ingress)
		dseqS, ok := ingress.Labels[akashLeaseDSeqLabelName]
		if !ok {
			return anError
		}
		gseqS, ok := ingress.Labels[akashLeaseGSeqLabelName]
		if !ok {
			return anError
		}
		oseqS, ok := ingress.Labels[akashLeaseOSeqLabelName]
		if !ok {
			return anError
		}
		owner, ok := ingress.Labels[akashLeaseOwnerLabelName]
		if !ok {
			return anError
		}

		provider, ok := ingress.Labels[akashLeaseProviderLabelName]
		if !ok {
			return anError
		}

		dseq, err := strconv.ParseUint(dseqS,10, 64)
		if err != nil {
			return err
		}

		gseq, err := strconv.ParseUint(gseqS, 10, 32)
		if err != nil {
			return err
		}

		oseq, err := strconv.ParseUint(oseqS, 10, 32)
		if err != nil {
			return err
		}

		ingressLeaseID := mtypes.LeaseID{
			Owner:    owner,
			DSeq:     dseq,
			GSeq:     uint32(gseq),
			OSeq:     uint32(oseq),
			Provider: provider,
		}
		if len(ingress.Spec.Rules) != 1 {
			// TODO - ???
		}
		rule := ingress.Spec.Rules[0]

		if len(rule.IngressRuleValue.HTTP.Paths) != 1 {
			// TODO - ???
		}
		rulePath := rule.IngressRuleValue.HTTP.Paths[0]
		results = append(results, leaseIdHostnameConnection{
			leaseID:      ingressLeaseID,
			hostname:     rule.Host,
			externalPort: rulePath.Backend.Service.Port.Number,
			serviceName:  rulePath.Backend.Service.Name,
		})

		return nil
	})

	if err != nil {
		return nil, err
	}

	return results, nil
}

func (c *client) AllHostnames(ctx context.Context) ([]cluster.ActiveHostname, error) {
	ingressPager := pager.New(func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error){
		return c.kc.NetworkingV1().Ingresses(metav1.NamespaceAll).List(ctx, opts)
	})

	listOptions := metav1.ListOptions{
		LabelSelector:        fmt.Sprintf("%s=true", akashManagedLabelName),
	}

	namespaces := make(map[string][]string, 0)
	err := ingressPager.EachListItem(ctx, listOptions, func(obj runtime.Object) error {
		ingress := obj.(*netv1.Ingress)
		namespace := ingress.Labels[akashNetworkNamespace]
		hostnames, _ := namespaces[namespace]
		for _, rule := range ingress.Spec.Rules {
			hostnames = append(hostnames, rule.Host)
		}
		namespaces[namespace] = hostnames
		return nil
	})

	if err != nil {
		return nil, err
	}

	result := make([]cluster.ActiveHostname, 0)
	nsPager := pager.New(func (ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return c.kc.CoreV1().Namespaces().List(ctx, opts)
	})
	err = nsPager.EachListItem(ctx, listOptions, func(obj runtime.Object) error {
		ns := obj.(*corev1.Namespace)
		hostnames, exists := namespaces[ns.Name]
		if !exists {
			return nil
		}

		owner, ok := ns.Labels[akashLeaseOwnerLabelName]
		if !ok || len(owner) == 0 {
			c.log.Error("namespace missing owner label", "ns", ns.Name)
			return nil
		}
		provider, ok := ns.Labels[akashLeaseProviderLabelName]
		if !ok || len(provider) == 0 {
			c.log.Error("namespace missing provider label", "ns", ns.Name)
			return nil
		}
		dseqStr, ok := ns.Labels[akashLeaseDSeqLabelName]
		if !ok {
			c.log.Error("namespace missing dseq label", "ns", ns.Name)
			return nil
		}
		gseqStr, ok := ns.Labels[akashLeaseGSeqLabelName]
		if !ok {
			c.log.Error("namespace missing gseq label", "ns", ns.Name)
			return nil
		}
		oseqStr, ok := ns.Labels[akashLeaseOSeqLabelName]
		if !ok {
			c.log.Error("namespace missing oseq label", "ns", ns.Name)
			return nil
		}
		dseq, err := strconv.ParseUint(dseqStr, 10, 64)
		if err != nil {
			c.log.Error("namespace dseq label invalid", "ns", ns.Name, "dseq", dseq)
			return nil
		}
		gseq, err := strconv.ParseUint(gseqStr, 10, 32)
		if err != nil {
			c.log.Error("namespace gseq label invalid", "ns", ns.Name, "gseq", gseq)
			return nil
		}
		oseq, err := strconv.ParseUint(oseqStr, 10, 32)
		if err != nil {
			c.log.Error("namespace oseq label invalid", "ns", ns.Name, "oseq", oseq)
			return nil
		}

		leaseID := mtypes.LeaseID{
			Owner:    owner,
			DSeq:     dseq,
			GSeq:     uint32(gseq),
			OSeq:     uint32(oseq),
			Provider: provider,
		}

		result = append(result, cluster.ActiveHostname{
			ID: leaseID,
			Hostnames: hostnames,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func ingressRules(hostname string, kubeServiceName string, kubeServicePort int32) []netv1.IngressRule{
	// for some reason we need to pass a pointer to this
	pathTypeForAll := netv1.PathTypePrefix
	ruleValue := netv1.HTTPIngressRuleValue{
		Paths: []netv1.HTTPIngressPath{{
			Path:     "/",
			PathType: &pathTypeForAll,
			Backend:  netv1.IngressBackend{
				Service:  &netv1.IngressServiceBackend{
					Name: kubeServiceName,
					Port: netv1.ServiceBackendPort{
						Number: kubeServicePort,
					},
				},
			},
		},},
	}

	return []netv1.IngressRule{{
		Host:             hostname,
		IngressRuleValue: netv1.IngressRuleValue{HTTP: &ruleValue},
	},}

}
