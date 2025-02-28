package webhook

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyverno/kyverno/api/kyverno"
	kyvernov1 "github.com/kyverno/kyverno/api/kyverno/v1"
	kyvernov2alpha1 "github.com/kyverno/kyverno/api/kyverno/v2alpha1"
	"github.com/kyverno/kyverno/ext/wildcard"
	"github.com/kyverno/kyverno/pkg/autogen"
	"github.com/kyverno/kyverno/pkg/client/clientset/versioned"
	kyvernov1informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v1"
	kyvernov2alpha1informers "github.com/kyverno/kyverno/pkg/client/informers/externalversions/kyverno/v2alpha1"
	kyvernov1listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v1"
	kyvernov2alpha1listers "github.com/kyverno/kyverno/pkg/client/listers/kyverno/v2alpha1"
	"github.com/kyverno/kyverno/pkg/clients/dclient"
	"github.com/kyverno/kyverno/pkg/config"
	"github.com/kyverno/kyverno/pkg/controllers"
	"github.com/kyverno/kyverno/pkg/tls"
	controllerutils "github.com/kyverno/kyverno/pkg/utils/controller"
	kubeutils "github.com/kyverno/kyverno/pkg/utils/kube"
	runtimeutils "github.com/kyverno/kyverno/pkg/utils/runtime"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	admissionregistrationv1informers "k8s.io/client-go/informers/admissionregistration/v1"
	coordinationv1informers "k8s.io/client-go/informers/coordination/v1"
	corev1informers "k8s.io/client-go/informers/core/v1"
	rbacv1informers "k8s.io/client-go/informers/rbac/v1"
	admissionregistrationv1listers "k8s.io/client-go/listers/admissionregistration/v1"
	coordinationv1listers "k8s.io/client-go/listers/coordination/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	rbacv1listers "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const (
	// Workers is the number of workers for this controller
	Workers                   = 2
	ControllerName            = "webhook-controller"
	DefaultWebhookTimeout     = 10
	AnnotationLastRequestTime = "kyverno.io/last-request-time"
	IdleDeadline              = tickerInterval * 10
	maxRetries                = 10
	tickerInterval            = 10 * time.Second
	webhookCreate             = "CREATE"
	webhookUpdate             = "UPDATE"
	webhookDelete             = "DELETE"
	webhookConnect            = "CONNECT"
)

var (
	none         = admissionregistrationv1.SideEffectClassNone
	noneOnDryRun = admissionregistrationv1.SideEffectClassNoneOnDryRun
	ifNeeded     = admissionregistrationv1.IfNeededReinvocationPolicy
	ignore       = admissionregistrationv1.Ignore
	fail         = admissionregistrationv1.Fail
	policyRule   = admissionregistrationv1.Rule{
		Resources:   []string{"clusterpolicies", "policies"},
		APIGroups:   []string{"kyverno.io"},
		APIVersions: []string{"v1", "v2beta1"},
	}
	verifyRule = admissionregistrationv1.Rule{
		Resources:   []string{"leases"},
		APIGroups:   []string{"coordination.k8s.io"},
		APIVersions: []string{"v1"},
	}
)

type controller struct {
	// clients
	discoveryClient dclient.IDiscovery
	mwcClient       controllerutils.ObjectClient[*admissionregistrationv1.MutatingWebhookConfiguration]
	vwcClient       controllerutils.ObjectClient[*admissionregistrationv1.ValidatingWebhookConfiguration]
	leaseClient     controllerutils.ObjectClient[*coordinationv1.Lease]
	kyvernoClient   versioned.Interface

	// listers
	mwcLister         admissionregistrationv1listers.MutatingWebhookConfigurationLister
	vwcLister         admissionregistrationv1listers.ValidatingWebhookConfigurationLister
	cpolLister        kyvernov1listers.ClusterPolicyLister
	polLister         kyvernov1listers.PolicyLister
	secretLister      corev1listers.SecretLister
	leaseLister       coordinationv1listers.LeaseLister
	clusterroleLister rbacv1listers.ClusterRoleLister
	gctxentryLister   kyvernov2alpha1listers.GlobalContextEntryLister

	// queue
	queue workqueue.RateLimitingInterface

	// config
	server             string
	defaultTimeout     int32
	servicePort        int32
	autoUpdateWebhooks bool
	admissionReports   bool
	runtime            runtimeutils.Runtime
	configuration      config.Configuration
	caSecretName       string

	// state
	lock        sync.Mutex
	policyState map[string]sets.Set[string]
}

func NewController(
	discoveryClient dclient.IDiscovery,
	mwcClient controllerutils.ObjectClient[*admissionregistrationv1.MutatingWebhookConfiguration],
	vwcClient controllerutils.ObjectClient[*admissionregistrationv1.ValidatingWebhookConfiguration],
	leaseClient controllerutils.ObjectClient[*coordinationv1.Lease],
	kyvernoClient versioned.Interface,
	mwcInformer admissionregistrationv1informers.MutatingWebhookConfigurationInformer,
	vwcInformer admissionregistrationv1informers.ValidatingWebhookConfigurationInformer,
	cpolInformer kyvernov1informers.ClusterPolicyInformer,
	polInformer kyvernov1informers.PolicyInformer,
	secretInformer corev1informers.SecretInformer,
	leaseInformer coordinationv1informers.LeaseInformer,
	clusterroleInformer rbacv1informers.ClusterRoleInformer,
	gctxentryInformer kyvernov2alpha1informers.GlobalContextEntryInformer,
	server string,
	defaultTimeout int32,
	servicePort int32,
	webhookServerPort int32,
	autoUpdateWebhooks bool,
	admissionReports bool,
	runtime runtimeutils.Runtime,
	configuration config.Configuration,
	caSecretName string,
) controllers.Controller {
	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ControllerName)
	c := controller{
		discoveryClient:    discoveryClient,
		mwcClient:          mwcClient,
		vwcClient:          vwcClient,
		leaseClient:        leaseClient,
		kyvernoClient:      kyvernoClient,
		mwcLister:          mwcInformer.Lister(),
		vwcLister:          vwcInformer.Lister(),
		cpolLister:         cpolInformer.Lister(),
		polLister:          polInformer.Lister(),
		secretLister:       secretInformer.Lister(),
		leaseLister:        leaseInformer.Lister(),
		clusterroleLister:  clusterroleInformer.Lister(),
		gctxentryLister:    gctxentryInformer.Lister(),
		queue:              queue,
		server:             server,
		defaultTimeout:     defaultTimeout,
		servicePort:        servicePort,
		autoUpdateWebhooks: autoUpdateWebhooks,
		admissionReports:   admissionReports,
		runtime:            runtime,
		configuration:      configuration,
		caSecretName:       caSecretName,
		policyState: map[string]sets.Set[string]{
			config.MutatingWebhookConfigurationName:   sets.New[string](),
			config.ValidatingWebhookConfigurationName: sets.New[string](),
		},
	}
	if _, _, err := controllerutils.AddDefaultEventHandlers(logger, mwcInformer.Informer(), queue); err != nil {
		logger.Error(err, "failed to register event handlers")
	}
	if _, _, err := controllerutils.AddDefaultEventHandlers(logger, vwcInformer.Informer(), queue); err != nil {
		logger.Error(err, "failed to register event handlers")
	}
	if _, err := controllerutils.AddEventHandlersT(
		secretInformer.Informer(),
		func(obj *corev1.Secret) {
			if obj.GetNamespace() == config.KyvernoNamespace() && obj.GetName() == caSecretName {
				c.enqueueAll()
			}
		},
		func(_, obj *corev1.Secret) {
			if obj.GetNamespace() == config.KyvernoNamespace() && obj.GetName() == caSecretName {
				c.enqueueAll()
			}
		},
		func(obj *corev1.Secret) {
			if obj.GetNamespace() == config.KyvernoNamespace() && obj.GetName() == caSecretName {
				c.enqueueAll()
			}
		},
	); err != nil {
		logger.Error(err, "failed to register event handlers")
	}
	if _, err := controllerutils.AddEventHandlers(
		cpolInformer.Informer(),
		func(interface{}) { c.enqueueResourceWebhooks(0) },
		func(interface{}, interface{}) { c.enqueueResourceWebhooks(0) },
		func(interface{}) { c.enqueueResourceWebhooks(0) },
	); err != nil {
		logger.Error(err, "failed to register event handlers")
	}
	if _, err := controllerutils.AddEventHandlers(
		polInformer.Informer(),
		func(interface{}) { c.enqueueResourceWebhooks(0) },
		func(interface{}, interface{}) { c.enqueueResourceWebhooks(0) },
		func(interface{}) { c.enqueueResourceWebhooks(0) },
	); err != nil {
		logger.Error(err, "failed to register event handlers")
	}
	configuration.OnChanged(c.enqueueAll)
	return &c
}

func (c *controller) Run(ctx context.Context, workers int) {
	// add our known webhooks to the queue
	c.enqueueAll()
	controllerutils.Run(ctx, logger, ControllerName, time.Second, c.queue, workers, maxRetries, c.reconcile, c.watchdog)
}

func (c *controller) watchdog(ctx context.Context, logger logr.Logger) {
	ticker := time.NewTicker(tickerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			lease, err := c.getLease()
			if err != nil {
				if apierrors.IsNotFound(err) {
					_, err = c.leaseClient.Create(ctx, &coordinationv1.Lease{
						ObjectMeta: metav1.ObjectMeta{
							Name:      "kyverno-health",
							Namespace: config.KyvernoNamespace(),
							Labels: map[string]string{
								"app.kubernetes.io/name": kyverno.ValueKyvernoApp,
							},
							Annotations: map[string]string{
								AnnotationLastRequestTime: time.Now().Format(time.RFC3339),
							},
						},
					}, metav1.CreateOptions{})
					if err != nil {
						logger.Error(err, "failed to create lease")
					}
				} else {
					logger.Error(err, "failed to get lease")
				}
			} else {
				lease := lease.DeepCopy()
				lease.Labels = map[string]string{
					"app.kubernetes.io/name": kyverno.ValueKyvernoApp,
				}
				_, err = c.leaseClient.Update(ctx, lease, metav1.UpdateOptions{})
				if err != nil {
					logger.Error(err, "failed to update lease")
				}
			}
			c.enqueueResourceWebhooks(0)
		}
	}
}

func (c *controller) watchdogCheck() bool {
	lease, err := c.getLease()
	if err != nil {
		logger.Error(err, "failed to get lease")
		return false
	}
	annotations := lease.GetAnnotations()
	if annotations == nil {
		return false
	}
	annTime, err := time.Parse(time.RFC3339, annotations[AnnotationLastRequestTime])
	if err != nil {
		return false
	}
	return time.Now().Before(annTime.Add(IdleDeadline))
}

func (c *controller) enqueueAll() {
	c.enqueuePolicyWebhooks()
	c.enqueueResourceWebhooks(0)
	c.enqueueVerifyWebhook()
}

func (c *controller) enqueuePolicyWebhooks() {
	c.queue.Add(config.PolicyValidatingWebhookConfigurationName)
	c.queue.Add(config.PolicyMutatingWebhookConfigurationName)
}

func (c *controller) enqueueResourceWebhooks(duration time.Duration) {
	c.queue.AddAfter(config.MutatingWebhookConfigurationName, duration)
	c.queue.AddAfter(config.ValidatingWebhookConfigurationName, duration)
}

func (c *controller) enqueueVerifyWebhook() {
	c.queue.Add(config.VerifyMutatingWebhookConfigurationName)
}

func (c *controller) recordPolicyState(webhookConfigurationName string, policies ...kyvernov1.PolicyInterface) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if _, ok := c.policyState[webhookConfigurationName]; !ok {
		return
	}
	c.policyState[webhookConfigurationName] = sets.New[string]()
	for _, policy := range policies {
		policyKey, err := cache.MetaNamespaceKeyFunc(policy)
		if err != nil {
			logger.Error(err, "failed to compute policy key", "policy", policy)
		} else {
			c.policyState[webhookConfigurationName].Insert(policyKey)
		}
	}
}

func (c *controller) clientConfig(caBundle []byte, path string) admissionregistrationv1.WebhookClientConfig {
	clientConfig := admissionregistrationv1.WebhookClientConfig{
		CABundle: caBundle,
	}
	if c.server == "" {
		clientConfig.Service = &admissionregistrationv1.ServiceReference{
			Namespace: config.KyvernoNamespace(),
			Name:      config.KyvernoServiceName(),
			Path:      &path,
			Port:      &c.servicePort,
		}
	} else {
		url := fmt.Sprintf("https://%s%s", c.server, path)
		clientConfig.URL = &url
	}
	return clientConfig
}

func (c *controller) reconcileResourceValidatingWebhookConfiguration(ctx context.Context) error {
	if c.autoUpdateWebhooks {
		return c.reconcileValidatingWebhookConfiguration(ctx, c.autoUpdateWebhooks, c.buildResourceValidatingWebhookConfiguration)
	} else {
		return c.reconcileValidatingWebhookConfiguration(ctx, c.autoUpdateWebhooks, c.buildDefaultResourceValidatingWebhookConfiguration)
	}
}

func (c *controller) reconcileResourceMutatingWebhookConfiguration(ctx context.Context) error {
	if c.autoUpdateWebhooks {
		return c.reconcileMutatingWebhookConfiguration(ctx, c.autoUpdateWebhooks, c.buildResourceMutatingWebhookConfiguration)
	} else {
		return c.reconcileMutatingWebhookConfiguration(ctx, c.autoUpdateWebhooks, c.buildDefaultResourceMutatingWebhookConfiguration)
	}
}

func (c *controller) reconcilePolicyValidatingWebhookConfiguration(ctx context.Context) error {
	return c.reconcileValidatingWebhookConfiguration(ctx, true, c.buildPolicyValidatingWebhookConfiguration)
}

func (c *controller) reconcilePolicyMutatingWebhookConfiguration(ctx context.Context) error {
	return c.reconcileMutatingWebhookConfiguration(ctx, true, c.buildPolicyMutatingWebhookConfiguration)
}

func (c *controller) reconcileVerifyMutatingWebhookConfiguration(ctx context.Context) error {
	return c.reconcileMutatingWebhookConfiguration(ctx, true, c.buildVerifyMutatingWebhookConfiguration)
}

func (c *controller) reconcileValidatingWebhookConfiguration(ctx context.Context, autoUpdateWebhooks bool, build func(context.Context, config.Configuration, []byte) (*admissionregistrationv1.ValidatingWebhookConfiguration, error)) error {
	caData, err := tls.ReadRootCASecret(c.caSecretName, config.KyvernoNamespace(), c.secretLister.Secrets(config.KyvernoNamespace()))
	if err != nil {
		return err
	}
	desired, err := build(ctx, c.configuration, caData)
	if err != nil {
		return err
	}
	observed, err := c.vwcLister.Get(desired.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, err := c.vwcClient.Create(ctx, desired, metav1.CreateOptions{})
			return err
		}
		return err
	}
	if !autoUpdateWebhooks {
		return nil
	}
	_, err = controllerutils.Update(ctx, observed, c.vwcClient, func(w *admissionregistrationv1.ValidatingWebhookConfiguration) error {
		w.Labels = desired.Labels
		w.Annotations = desired.Annotations
		w.OwnerReferences = desired.OwnerReferences
		w.Webhooks = desired.Webhooks
		return nil
	})
	return err
}

func (c *controller) reconcileMutatingWebhookConfiguration(ctx context.Context, autoUpdateWebhooks bool, build func(context.Context, config.Configuration, []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, error)) error {
	caData, err := tls.ReadRootCASecret(c.caSecretName, config.KyvernoNamespace(), c.secretLister.Secrets(config.KyvernoNamespace()))
	if err != nil {
		return err
	}
	desired, err := build(ctx, c.configuration, caData)
	if err != nil {
		return err
	}
	observed, err := c.mwcLister.Get(desired.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, err := c.mwcClient.Create(ctx, desired, metav1.CreateOptions{})
			return err
		}
		return err
	}
	if !autoUpdateWebhooks {
		return nil
	}
	_, err = controllerutils.Update(ctx, observed, c.mwcClient, func(w *admissionregistrationv1.MutatingWebhookConfiguration) error {
		w.Labels = desired.Labels
		w.Annotations = desired.Annotations
		w.OwnerReferences = desired.OwnerReferences
		w.Webhooks = desired.Webhooks
		return nil
	})
	return err
}

func (c *controller) isGlobalContextEntryReady(name string, gctxentries []*kyvernov2alpha1.GlobalContextEntry) bool {
	for _, gctxentry := range gctxentries {
		if gctxentry.Name == name {
			return gctxentry.Status.Ready
		}
	}
	return false
}

func (c *controller) updatePolicyStatuses(ctx context.Context) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	policies, err := c.getAllPolicies()
	if err != nil {
		return err
	}
	gctxentries, err := c.gctxentryLister.List(labels.Everything())
	if err != nil {
		return err
	}
	updateStatusFunc := func(policy kyvernov1.PolicyInterface) error {
		policyKey, err := cache.MetaNamespaceKeyFunc(policy)
		if err != nil {
			return err
		}
		ready, message := true, "Ready"
		if c.autoUpdateWebhooks {
			for _, set := range c.policyState {
				if !set.Has(policyKey) {
					ready, message = false, "Not ready yet"
					break
				}
			}
		}
		// If there are global context entries under , check if they are ready
		if ready {
			for _, rule := range policy.GetSpec().Rules {
				if rule.Context == nil {
					continue
				}
				for _, ctxEntry := range rule.Context {
					if ctxEntry.GlobalReference != nil {
						if !c.isGlobalContextEntryReady(ctxEntry.GlobalReference.Name, gctxentries) {
							ready, message = false, "Not ready yet"
							break
						}
					}
				}
			}
		}
		status := policy.GetStatus()
		status.SetReady(ready, message)
		status.Autogen.Rules = nil
		rules := autogen.ComputeRules(policy)
		setRuleCount(rules, status)
		for _, rule := range rules {
			if strings.HasPrefix(rule.Name, "autogen-") {
				status.Autogen.Rules = append(status.Autogen.Rules, rule)
			}
		}
		return nil
	}
	for _, policy := range policies {
		if policy.GetNamespace() == "" {
			_, err := controllerutils.UpdateStatus(
				ctx,
				policy.(*kyvernov1.ClusterPolicy),
				c.kyvernoClient.KyvernoV1().ClusterPolicies(),
				func(policy *kyvernov1.ClusterPolicy) error {
					return updateStatusFunc(policy)
				},
			)
			if err != nil {
				return err
			}
		} else {
			_, err := controllerutils.UpdateStatus(
				ctx,
				policy.(*kyvernov1.Policy),
				c.kyvernoClient.KyvernoV1().Policies(policy.GetNamespace()),
				func(policy *kyvernov1.Policy) error {
					return updateStatusFunc(policy)
				},
			)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *controller) reconcile(ctx context.Context, logger logr.Logger, key, namespace, name string) error {
	switch name {
	case config.MutatingWebhookConfigurationName:
		if c.runtime.IsRollingUpdate() {
			c.enqueueResourceWebhooks(1 * time.Second)
		} else {
			if err := c.reconcileResourceMutatingWebhookConfiguration(ctx); err != nil {
				return err
			}
			if err := c.updatePolicyStatuses(ctx); err != nil {
				return err
			}
		}
	case config.ValidatingWebhookConfigurationName:
		if c.runtime.IsRollingUpdate() {
			c.enqueueResourceWebhooks(1 * time.Second)
		} else {
			if err := c.reconcileResourceValidatingWebhookConfiguration(ctx); err != nil {
				return err
			}
			if err := c.updatePolicyStatuses(ctx); err != nil {
				return err
			}
		}
	case config.PolicyValidatingWebhookConfigurationName:
		return c.reconcilePolicyValidatingWebhookConfiguration(ctx)
	case config.PolicyMutatingWebhookConfigurationName:
		return c.reconcilePolicyMutatingWebhookConfiguration(ctx)
	case config.VerifyMutatingWebhookConfigurationName:
		return c.reconcileVerifyMutatingWebhookConfiguration(ctx)
	}
	return nil
}

func (c *controller) buildVerifyMutatingWebhookConfiguration(_ context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: objectMeta(config.VerifyMutatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name:         config.VerifyMutatingWebhookName,
				ClientConfig: c.clientConfig(caBundle, config.VerifyMutatingWebhookServicePath),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: verifyRule,
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Update,
					},
				}},
				FailurePolicy:           &ignore,
				SideEffects:             &noneOnDryRun,
				TimeoutSeconds:          &c.defaultTimeout,
				ReinvocationPolicy:      &ifNeeded,
				AdmissionReviewVersions: []string{"v1"},
				ObjectSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": kyverno.ValueKyvernoApp,
					},
				},
			}},
		},
		nil
}

func (c *controller) buildPolicyMutatingWebhookConfiguration(_ context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: objectMeta(config.PolicyMutatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name:         config.PolicyMutatingWebhookName,
				ClientConfig: c.clientConfig(caBundle, config.PolicyMutatingWebhookServicePath),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: policyRule,
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
				}},
				FailurePolicy:           &fail,
				TimeoutSeconds:          &c.defaultTimeout,
				SideEffects:             &noneOnDryRun,
				ReinvocationPolicy:      &ifNeeded,
				AdmissionReviewVersions: []string{"v1"},
			}},
		},
		nil
}

func (c *controller) buildPolicyValidatingWebhookConfiguration(_ context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: objectMeta(config.PolicyValidatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name:         config.PolicyValidatingWebhookName,
				ClientConfig: c.clientConfig(caBundle, config.PolicyValidatingWebhookServicePath),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: policyRule,
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
				}},
				FailurePolicy:           &fail,
				TimeoutSeconds:          &c.defaultTimeout,
				SideEffects:             &none,
				AdmissionReviewVersions: []string{"v1"},
			}},
		},
		nil
}

func (c *controller) buildDefaultResourceMutatingWebhookConfiguration(_ context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	return &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: objectMeta(config.MutatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name:         config.MutatingWebhookName + "-ignore",
				ClientConfig: c.clientConfig(caBundle, config.MutatingWebhookServicePath+"/ignore"),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Resources:   []string{"*/*"},
					},
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
				}},
				FailurePolicy:           &ignore,
				SideEffects:             &noneOnDryRun,
				AdmissionReviewVersions: []string{"v1"},
				TimeoutSeconds:          &c.defaultTimeout,
				ReinvocationPolicy:      &ifNeeded,
			}, {
				Name:         config.MutatingWebhookName + "-fail",
				ClientConfig: c.clientConfig(caBundle, config.MutatingWebhookServicePath+"/fail"),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Resources:   []string{"*/*"},
					},
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
				}},
				FailurePolicy:           &fail,
				SideEffects:             &noneOnDryRun,
				AdmissionReviewVersions: []string{"v1"},
				TimeoutSeconds:          &c.defaultTimeout,
				ReinvocationPolicy:      &ifNeeded,
			}},
		},
		nil
}

func (c *controller) buildResourceMutatingWebhookConfiguration(ctx context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.MutatingWebhookConfiguration, error) {
	result := admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: objectMeta(config.MutatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
		Webhooks:   []admissionregistrationv1.MutatingWebhook{},
	}
	var mapResourceToOpnType map[string][]admissionregistrationv1.OperationType
	if c.watchdogCheck() {
		webhookCfg := config.WebhookConfig{}
		webhookCfgs := cfg.GetWebhooks()
		if len(webhookCfgs) > 0 {
			webhookCfg = webhookCfgs[0]
		}

		ignoreWebhook := newWebhook(c.defaultTimeout, ignore, cfg.GetMatchConditions())
		failWebhook := newWebhook(c.defaultTimeout, fail, cfg.GetMatchConditions())
		policies, err := c.getAllPolicies()
		if err != nil {
			return nil, err
		}
		var fineGrainedIgnoreList, fineGrainedFailList []*webhook
		c.recordPolicyState(config.MutatingWebhookConfigurationName, policies...)
		for _, p := range policies {
			if p.AdmissionProcessingEnabled() {
				spec := p.GetSpec()
				if spec.HasMutateStandard() || spec.HasVerifyImages() {
					if spec.CustomWebhookConfiguration() {
						fineGrainedIgnore := newWebhookPerPolicy(c.defaultTimeout, ignore, cfg.GetMatchConditions(), p)
						fineGrainedFail := newWebhookPerPolicy(c.defaultTimeout, fail, cfg.GetMatchConditions(), p)
						if spec.GetFailurePolicy(ctx) == kyvernov1.Ignore {
							c.mergeWebhook(fineGrainedIgnore, p, false)
							fineGrainedIgnoreList = append(fineGrainedIgnoreList, fineGrainedIgnore)
						} else {
							c.mergeWebhook(fineGrainedFail, p, false)
							fineGrainedFailList = append(fineGrainedFailList, fineGrainedFail)
						}
						continue
					}

					if spec.GetFailurePolicy(ctx) == kyvernov1.Ignore {
						c.mergeWebhook(ignoreWebhook, p, false)
					} else {
						c.mergeWebhook(failWebhook, p, false)
					}
					rules := p.GetSpec().Rules
					mapResourceToOpnType = addOpnForMutatingWebhookConf(rules, mapResourceToOpnType)
				}
			}
		}

		webhooks := []*webhook{ignoreWebhook, failWebhook}
		webhooks = append(webhooks, fineGrainedIgnoreList...)
		webhooks = append(webhooks, fineGrainedFailList...)
		result.Webhooks = c.buildResourceMutatingWebhookRules(caBundle, webhookCfg, &noneOnDryRun, webhooks, mapResourceToOpnType)
	} else {
		c.recordPolicyState(config.MutatingWebhookConfigurationName)
	}
	return &result, nil
}

func (c *controller) buildResourceMutatingWebhookRules(caBundle []byte, webhookCfg config.WebhookConfig, sideEffects *admissionregistrationv1.SideEffectClass, webhooks []*webhook, mapResourceToOpnType map[string][]admissionregistrationv1.OperationType) []admissionregistrationv1.MutatingWebhook {
	var mutatingWebhooks []admissionregistrationv1.MutatingWebhook
	for _, webhook := range webhooks {
		if webhook.isEmpty() {
			continue
		}
		failurePolicy := webhook.failurePolicy
		timeout := capTimeout(webhook.maxWebhookTimeout)
		name, path := webhookNameAndPath(*webhook, config.MutatingWebhookName, config.MutatingWebhookServicePath)
		mutatingWebhooks = append(
			mutatingWebhooks,
			admissionregistrationv1.MutatingWebhook{
				Name:                    name,
				ClientConfig:            c.clientConfig(caBundle, path),
				Rules:                   webhook.buildRulesWithOperations(mapResourceToOpnType, []admissionregistrationv1.OperationType{"CREATE", "UPDATE"}),
				FailurePolicy:           &failurePolicy,
				SideEffects:             sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				NamespaceSelector:       webhookCfg.NamespaceSelector,
				ObjectSelector:          webhookCfg.ObjectSelector,
				TimeoutSeconds:          &timeout,
				ReinvocationPolicy:      &ifNeeded,
				MatchConditions:         webhook.matchConditions,
			},
		)
	}
	return mutatingWebhooks
}

func (c *controller) buildDefaultResourceValidatingWebhookConfiguration(_ context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	sideEffects := &none
	if c.admissionReports {
		sideEffects = &noneOnDryRun
	}
	return &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: objectMeta(config.ValidatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name:         config.ValidatingWebhookName + "-ignore",
				ClientConfig: c.clientConfig(caBundle, config.ValidatingWebhookServicePath+"/ignore"),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Resources:   []string{"*/*"},
					},
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
						admissionregistrationv1.Delete,
						admissionregistrationv1.Connect,
					},
				}},
				FailurePolicy:           &ignore,
				SideEffects:             sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				TimeoutSeconds:          &c.defaultTimeout,
			}, {
				Name:         config.ValidatingWebhookName + "-fail",
				ClientConfig: c.clientConfig(caBundle, config.ValidatingWebhookServicePath+"/fail"),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"*"},
						APIVersions: []string{"*"},
						Resources:   []string{"*/*"},
					},
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
						admissionregistrationv1.Delete,
						admissionregistrationv1.Connect,
					},
				}},
				FailurePolicy:           &fail,
				SideEffects:             sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				TimeoutSeconds:          &c.defaultTimeout,
			}},
		},
		nil
}

func addOpnForMutatingWebhookConf(rules []kyvernov1.Rule, mapResourceToOpnType map[string][]admissionregistrationv1.OperationType) map[string][]admissionregistrationv1.OperationType {
	var mapResourceToOpn map[string]map[string]bool
	for _, r := range rules {
		var resources []string
		operationStatusMap := getOperationStatusMap()
		operationStatusMap = computeOperationsForMutatingWebhookConf(r, operationStatusMap)
		resources = computeResourcesOfRule(r)
		for _, r := range resources {
			mapResourceToOpn, mapResourceToOpnType = appendResource(r, mapResourceToOpn, operationStatusMap, mapResourceToOpnType)
		}
	}
	return mapResourceToOpnType
}

func addOpnForValidatingWebhookConf(rules []kyvernov1.Rule, mapResourceToOpnType map[string][]admissionregistrationv1.OperationType) map[string][]admissionregistrationv1.OperationType {
	var mapResourceToOpn map[string]map[string]bool
	for _, r := range rules {
		var resources []string
		operationStatusMap := getOperationStatusMap()
		operationStatusMap = computeOperationsForValidatingWebhookConf(r, operationStatusMap)
		resources = computeResourcesOfRule(r)
		for _, r := range resources {
			mapResourceToOpn, mapResourceToOpnType = appendResource(r, mapResourceToOpn, operationStatusMap, mapResourceToOpnType)
		}
	}
	return mapResourceToOpnType
}

func (c *controller) buildResourceValidatingWebhookConfiguration(ctx context.Context, cfg config.Configuration, caBundle []byte) (*admissionregistrationv1.ValidatingWebhookConfiguration, error) {
	result := admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: objectMeta(config.ValidatingWebhookConfigurationName, cfg.GetWebhookAnnotations(), cfg.GetWebhookLabels(), c.buildOwner()...),
		Webhooks:   []admissionregistrationv1.ValidatingWebhook{},
	}
	var mapResourceToOpnType map[string][]admissionregistrationv1.OperationType
	if c.watchdogCheck() {
		webhookCfg := config.WebhookConfig{}
		webhookCfgs := cfg.GetWebhooks()
		if len(webhookCfgs) > 0 {
			webhookCfg = webhookCfgs[0]
		}

		ignoreWebhook := newWebhook(c.defaultTimeout, ignore, cfg.GetMatchConditions())
		failWebhook := newWebhook(c.defaultTimeout, fail, cfg.GetMatchConditions())
		policies, err := c.getAllPolicies()
		if err != nil {
			return nil, err
		}

		var fineGrainedIgnoreList, fineGrainedFailList []*webhook
		c.recordPolicyState(config.ValidatingWebhookConfigurationName, policies...)
		for _, p := range policies {
			if p.AdmissionProcessingEnabled() {
				spec := p.GetSpec()
				if spec.HasValidate() || spec.HasGenerate() || spec.HasMutateExisting() || spec.HasVerifyImageChecks() || spec.HasVerifyManifests() {
					if spec.CustomWebhookConfiguration() {
						fineGrainedIgnore := newWebhookPerPolicy(c.defaultTimeout, ignore, cfg.GetMatchConditions(), p)
						fineGrainedFail := newWebhookPerPolicy(c.defaultTimeout, fail, cfg.GetMatchConditions(), p)
						if spec.GetFailurePolicy(ctx) == kyvernov1.Ignore {
							c.mergeWebhook(fineGrainedIgnore, p, true)
							fineGrainedIgnoreList = append(fineGrainedIgnoreList, fineGrainedIgnore)
						} else {
							c.mergeWebhook(fineGrainedFail, p, true)
							fineGrainedFailList = append(fineGrainedFailList, fineGrainedFail)
						}
						continue
					}

					if spec.GetFailurePolicy(ctx) == kyvernov1.Ignore {
						c.mergeWebhook(ignoreWebhook, p, true)
					} else {
						c.mergeWebhook(failWebhook, p, true)
					}
				}
			}
			rules := p.GetSpec().Rules
			mapResourceToOpnType = addOpnForValidatingWebhookConf(rules, mapResourceToOpnType)
		}

		sideEffects := &none
		if c.admissionReports {
			sideEffects = &noneOnDryRun
		}

		webhooks := []*webhook{ignoreWebhook, failWebhook}
		webhooks = append(webhooks, fineGrainedIgnoreList...)
		webhooks = append(webhooks, fineGrainedFailList...)
		result.Webhooks = c.buildResourceValidatingWebhookRules(caBundle, webhookCfg, sideEffects, webhooks, mapResourceToOpnType)
	} else {
		c.recordPolicyState(config.MutatingWebhookConfigurationName)
	}
	return &result, nil
}

func (c *controller) buildResourceValidatingWebhookRules(caBundle []byte, webhookCfg config.WebhookConfig, sideEffects *admissionregistrationv1.SideEffectClass, webhooks []*webhook, mapResourceToOpnType map[string][]admissionregistrationv1.OperationType) []admissionregistrationv1.ValidatingWebhook {
	var validatingWebhooks []admissionregistrationv1.ValidatingWebhook
	for _, webhook := range webhooks {
		if webhook.isEmpty() {
			continue
		}
		timeout := capTimeout(webhook.maxWebhookTimeout)
		name, path := webhookNameAndPath(*webhook, config.ValidatingWebhookName, config.ValidatingWebhookServicePath)
		failurePolicy := webhook.failurePolicy
		validatingWebhooks = append(
			validatingWebhooks,
			admissionregistrationv1.ValidatingWebhook{
				Name:                    name,
				ClientConfig:            c.clientConfig(caBundle, path),
				Rules:                   webhook.buildRulesWithOperations(mapResourceToOpnType, []admissionregistrationv1.OperationType{"CREATE", "UPDATE", "DELETE", "CONNECT"}),
				FailurePolicy:           &failurePolicy,
				SideEffects:             sideEffects,
				AdmissionReviewVersions: []string{"v1"},
				NamespaceSelector:       webhookCfg.NamespaceSelector,
				ObjectSelector:          webhookCfg.ObjectSelector,
				TimeoutSeconds:          &timeout,
				MatchConditions:         webhook.matchConditions,
			},
		)
	}
	return validatingWebhooks
}

func (c *controller) getAllPolicies() ([]kyvernov1.PolicyInterface, error) {
	var policies []kyvernov1.PolicyInterface
	if cpols, err := c.cpolLister.List(labels.Everything()); err != nil {
		return nil, err
	} else {
		for _, cpol := range cpols {
			policies = append(policies, cpol)
		}
	}
	if pols, err := c.polLister.List(labels.Everything()); err != nil {
		return nil, err
	} else {
		for _, pol := range pols {
			policies = append(policies, pol)
		}
	}
	return policies, nil
}

func (c *controller) getLease() (*coordinationv1.Lease, error) {
	return c.leaseLister.Leases(config.KyvernoNamespace()).Get("kyverno-health")
}

// GroupVersionResourceScope adds the resource scope to the GVR
type GroupVersionResourceScope struct {
	schema.GroupVersionResource
	Scope admissionregistrationv1.ScopeType
}

// String puts / between group/version/resource and scope
func (gvs GroupVersionResourceScope) String() string {
	return gvs.GroupVersion().String() + "/" + gvs.Resource + "/" + string(gvs.Scope)
}

// mergeWebhook merges the matching kinds of the policy to webhook.rule
func (c *controller) mergeWebhook(dst *webhook, policy kyvernov1.PolicyInterface, updateValidate bool) {
	var matchedGVK []string
	for _, rule := range autogen.ComputeRules(policy) {
		// matching kinds in generate policies need to be added to both webhook
		if rule.HasGenerate() {
			matchedGVK = append(matchedGVK, rule.MatchResources.GetKinds()...)
			if rule.Generation.ResourceSpec.Kind != "" {
				matchedGVK = append(matchedGVK, rule.Generation.ResourceSpec.Kind)
			}
			matchedGVK = append(matchedGVK, rule.Generation.CloneList.Kinds...)
			continue
		}
		if (updateValidate && rule.HasValidate() || rule.HasVerifyImageChecks()) ||
			(updateValidate && rule.HasMutateExisting()) ||
			(!updateValidate && rule.HasMutateStandard()) ||
			(!updateValidate && rule.HasVerifyImages()) || (!updateValidate && rule.HasVerifyManifests()) {
			matchedGVK = append(matchedGVK, rule.MatchResources.GetKinds()...)
		}
	}
	var gvrsList []GroupVersionResourceScope
	for _, gvk := range matchedGVK {
		// NOTE: webhook stores GVR in its rules while policy stores GVK in its rules definition
		group, version, kind, subresource := kubeutils.ParseKindSelector(gvk)

		// if kind or group is `*` we use the scope of the policy
		policyScope := admissionregistrationv1.AllScopes
		if policy.IsNamespaced() {
			policyScope = admissionregistrationv1.NamespacedScope
		}

		// if kind is `*` no need to lookup resources
		if kind == "*" && subresource == "*" {
			gvrsList = append(gvrsList, GroupVersionResourceScope{GroupVersionResource: schema.GroupVersionResource{Group: group, Version: version, Resource: "*/*"}, Scope: policyScope})
		} else if kind == "*" && subresource == "" {
			gvrsList = append(gvrsList, GroupVersionResourceScope{GroupVersionResource: schema.GroupVersionResource{Group: group, Version: version, Resource: "*"}, Scope: policyScope})
		} else if kind == "*" && subresource != "" {
			gvrsList = append(gvrsList, GroupVersionResourceScope{GroupVersionResource: schema.GroupVersionResource{Group: group, Version: version, Resource: "*/" + subresource}, Scope: policyScope})
		} else {
			gvrss, err := c.discoveryClient.FindResources(group, version, kind, subresource)
			if err != nil {
				logger.Error(err, "unable to find resource", "group", group, "version", version, "kind", kind, "subresource", subresource)
				continue
			}
			for gvrs, resource := range gvrss {
				resourceScope := admissionregistrationv1.AllScopes
				if resource.Namespaced {
					resourceScope = admissionregistrationv1.NamespacedScope
				}
				gvrsList = append(gvrsList, GroupVersionResourceScope{GroupVersionResource: gvrs.GroupVersion.WithResource(gvrs.ResourceSubresource()), Scope: resourceScope})
			}
		}
	}
	for _, gvrs := range gvrsList {
		dst.set(gvrs)
	}

	spec := policy.GetSpec()
	if spec.WebhookTimeoutSeconds != nil {
		if dst.maxWebhookTimeout < *spec.WebhookTimeoutSeconds {
			dst.maxWebhookTimeout = *spec.WebhookTimeoutSeconds
		}
	}
}

func (c *controller) buildOwner() []metav1.OwnerReference {
	selector := labels.SelectorFromSet(labels.Set(map[string]string{
		kyverno.LabelAppComponent: "kyverno",
	}))

	clusterroles, err := c.clusterroleLister.List(selector)
	if err != nil {
		logger.Error(err, "failed to fetch kyverno clusterroles, won't set owners for webhook configurations")
		return nil
	}

	for _, clusterrole := range clusterroles {
		if wildcard.Match("*:webhook", clusterrole.GetName()) {
			return []metav1.OwnerReference{
				{
					APIVersion: "rbac.authorization.k8s.io/v1",
					Kind:       "ClusterRole",
					Name:       clusterrole.GetName(),
					UID:        clusterrole.GetUID(),
				},
			}
		}
	}
	return nil
}
