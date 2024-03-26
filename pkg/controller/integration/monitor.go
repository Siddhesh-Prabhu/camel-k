/*
Licensed to the Apache Software Foundation (ASF) under one or more
contributor license agreements.  See the NOTICE file distributed with
this work for additional information regarding copyright ownership.
The ASF licenses this file to You under the Apache License, Version 2.0
(the "License"); you may not use this file except in compliance with
the License.  You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/utils/pointer"

	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	servingv1 "knative.dev/serving/pkg/apis/serving/v1"

	v1 "github.com/apache/camel-k/v2/pkg/apis/camel/v1"
	"github.com/apache/camel-k/v2/pkg/client"
	"github.com/apache/camel-k/v2/pkg/trait"
	"github.com/apache/camel-k/v2/pkg/util/digest"
	"github.com/apache/camel-k/v2/pkg/util/kubernetes"
	utilResource "github.com/apache/camel-k/v2/pkg/util/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewMonitorAction is an action used to monitor manager Integrations.
func NewMonitorAction() Action {
	return &monitorAction{}
}

type monitorAction struct {
	baseAction
}

func (action *monitorAction) Name() string {
	return "monitor"
}

func (action *monitorAction) CanHandle(integration *v1.Integration) bool {
	return integration.Status.Phase == v1.IntegrationPhaseDeploying ||
		integration.Status.Phase == v1.IntegrationPhaseRunning ||
		integration.Status.Phase == v1.IntegrationPhaseError
}

func (action *monitorAction) Handle(ctx context.Context, integration *v1.Integration) (*v1.Integration, error) {
	// When in InitializationFailed condition a kit is not available for the integration
	// so handle it differently from the rest
	if isInInitializationFailed(integration.Status) {
		// Only check if the Integration requires a rebuild
		return action.checkDigestAndRebuild(ctx, integration, nil)
	}

	// At this stage the Integration must have a Kit
	if integration.Status.IntegrationKit == nil {
		return nil, fmt.Errorf("no kit set on integration %s", integration.Name)
	}

	kit, err := kubernetes.GetIntegrationKit(ctx, action.client,
		integration.Status.IntegrationKit.Name, integration.Status.IntegrationKit.Namespace)
	if err != nil {
		return nil, fmt.Errorf("unable to find integration kit %s/%s: %w",
			integration.Status.IntegrationKit.Namespace, integration.Status.IntegrationKit.Name, err)
	}

	// If integration is in error and its kit is also in error then integration does not change
	if isInIntegrationKitFailed(integration.Status) &&
		kit.Status.Phase == v1.IntegrationKitPhaseError {
		return nil, nil
	}

	// Check if the Integration requires a rebuild
	if changed, err := action.checkDigestAndRebuild(ctx, integration, kit); err != nil {
		return nil, err
	} else if changed != nil {
		return changed, nil
	}

	// Check if an IntegrationKit with higher priority is ready
	priority, ok := kit.Labels[v1.IntegrationKitPriorityLabel]
	if !ok {
		priority = "0"
	}
	withHigherPriority, err := labels.NewRequirement(v1.IntegrationKitPriorityLabel,
		selection.GreaterThan, []string{priority})
	if err != nil {
		return nil, err
	}

	kits, err := lookupKitsForIntegration(ctx, action.client, integration, ctrl.MatchingLabelsSelector{
		Selector: labels.NewSelector().Add(*withHigherPriority),
	})
	if err != nil {
		return nil, err
	}
	priorityReadyKit, err := findHighestPriorityReadyKit(kits)
	if err != nil {
		return nil, err
	}
	if priorityReadyKit != nil {
		integration.SetIntegrationKit(priorityReadyKit)
	}

	// Run traits that are enabled for the phase
	environment, err := trait.Apply(ctx, action.client, integration, kit)
	if err != nil {
		integration.Status.Phase = v1.IntegrationPhaseError
		integration.SetReadyCondition(corev1.ConditionFalse,
			v1.IntegrationConditionInitializationFailedReason, err.Error())
		return integration, err
	}

	return action.monitorPods(ctx, environment, integration)
}

func (action *monitorAction) monitorPods(ctx context.Context, environment *trait.Environment, integration *v1.Integration) (*v1.Integration, error) {
	controller, err := action.newController(environment, integration)
	if err != nil {
		return nil, err
	}

	// In order to simplify the monitoring and have a minor resource requirement, we will watch only those Pods
	// which are labeled with `camel.apache.org/integration`. This is a design choice that requires the user to
	// voluntarily add a label to their Pods (via template, possibly) in order to monitor the non managed Camel applications.

	if !controller.hasTemplateIntegrationLabel() {
		// This is happening when the Deployment, CronJob, etc resources
		// miss the Integration label, required to identify sibling Pods.
		integration.Status.SetConditions(
			v1.IntegrationCondition{
				Type:   v1.IntegrationConditionReady,
				Status: corev1.ConditionFalse,
				Reason: v1.IntegrationConditionMonitoringPodsAvailableReason,
				Message: fmt.Sprintf(
					"Could not find `camel.apache.org/integration: %s` label in the %s template. Make sure to include this label in the template for Pod monitoring purposes.",
					integration.GetName(),
					controller.getControllerName(),
				),
			},
		)
		return integration, nil
	}

	// Enforce the scale sub-resource label selector.
	// It is used by the HPA that queries the scale sub-resource endpoint,
	// to list the pods owned by the integration.
	integration.Status.Selector = v1.IntegrationLabel + "=" + integration.Name

	// Update the replicas count
	pendingPods := &corev1.PodList{}
	err = action.client.List(ctx, pendingPods,
		ctrl.InNamespace(integration.Namespace),
		ctrl.MatchingLabels{v1.IntegrationLabel: integration.Name},
		ctrl.MatchingFields{"status.phase": string(corev1.PodPending)})
	if err != nil {
		return nil, err
	}
	runningPods := &corev1.PodList{}
	err = action.client.List(ctx, runningPods,
		ctrl.InNamespace(integration.Namespace),
		ctrl.MatchingLabels{v1.IntegrationLabel: integration.Name},
		ctrl.MatchingFields{"status.phase": string(corev1.PodRunning)})
	if err != nil {
		return nil, err
	}
	nonTerminatingPods := 0
	for _, pod := range runningPods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		nonTerminatingPods++
	}
	podCount := int32(len(pendingPods.Items) + nonTerminatingPods)
	integration.Status.Replicas = &podCount

	// Reconcile Integration phase and ready condition
	if integration.Status.Phase == v1.IntegrationPhaseDeploying {
		integration.Status.Phase = v1.IntegrationPhaseRunning
	}
	if err = action.updateIntegrationPhaseAndReadyCondition(
		ctx, controller, environment, integration, pendingPods.Items, runningPods.Items,
	); err != nil {
		return nil, err
	}

	return integration, nil
}

func isInInitializationFailed(status v1.IntegrationStatus) bool {
	if status.Phase != v1.IntegrationPhaseError {
		return false
	}
	if cond := status.GetCondition(v1.IntegrationConditionReady); cond != nil {
		if cond.Status == corev1.ConditionFalse &&
			cond.Reason == v1.IntegrationConditionInitializationFailedReason {
			return true
		}
	}

	return false
}

func isInIntegrationKitFailed(status v1.IntegrationStatus) bool {
	if cond := status.GetCondition(v1.IntegrationConditionKitAvailable); cond != nil {
		if cond.Status == corev1.ConditionFalse &&
			status.Phase != v1.IntegrationPhaseError {
			return true
		}
	}

	return false
}

func (action *monitorAction) checkDigestAndRebuild(ctx context.Context, integration *v1.Integration, kit *v1.IntegrationKit) (*v1.Integration, error) {
	secrets, configmaps := getIntegrationSecretAndConfigmapResourceVersions(ctx, action.client, integration)
	hash, err := digest.ComputeForIntegration(integration, configmaps, secrets)
	if err != nil {
		return nil, err
	}

	if hash != integration.Status.Digest {
		action.L.Infof("Integration %s digest has changed: resetting its status. Will check if it needs to be rebuilt and restarted.", integration.Name)
		if isIntegrationKitResetRequired(integration, kit) {
			integration.SetIntegrationKit(nil)
		}
		integration.Initialize()
		integration.Status.Digest = hash

		return integration, nil
	}

	return nil, nil
}

func isIntegrationKitResetRequired(integration *v1.Integration, kit *v1.IntegrationKit) bool {
	if kit == nil {
		return false
	}

	if v1.GetOperatorIDAnnotation(integration) != "" &&
		v1.GetOperatorIDAnnotation(integration) != v1.GetOperatorIDAnnotation(kit) {
		// Operator to reconcile the integration has changed. Reset integration kit
		// so new operator can handle the kit reference
		return true
	}

	if v1.GetIntegrationProfileAnnotation(integration) != "" &&
		v1.GetIntegrationProfileAnnotation(integration) != v1.GetIntegrationProfileAnnotation(kit) {
		// Integration profile for the integration has changed. Reset integration kit
		// so new profile can be applied
		return true
	}

	if v1.GetIntegrationProfileNamespaceAnnotation(integration) != "" &&
		v1.GetIntegrationProfileNamespaceAnnotation(integration) != v1.GetIntegrationProfileNamespaceAnnotation(kit) {
		// Integration profile namespace for the integration has changed. Reset integration kit
		// so new profile can be applied
		return true
	}

	return false
}

// getIntegrationSecretAndConfigmapResourceVersions returns the list of resource versions only useful to watch for changes.
func getIntegrationSecretAndConfigmapResourceVersions(ctx context.Context, client client.Client, integration *v1.Integration) ([]string, []string) {
	configmaps := make([]string, 0)
	secrets := make([]string, 0)
	if integration.Spec.Traits.Mount != nil && pointer.BoolDeref(integration.Spec.Traits.Mount.HotReload, false) {
		mergedResources := make([]string, 0)
		mergedResources = append(mergedResources, integration.Spec.Traits.Mount.Configs...)
		mergedResources = append(mergedResources, integration.Spec.Traits.Mount.Resources...)
		for _, c := range mergedResources {
			if conf, parseErr := utilResource.ParseConfig(c); parseErr == nil {
				if conf.StorageType() == utilResource.StorageTypeConfigmap {
					cm := corev1.ConfigMap{
						TypeMeta: metav1.TypeMeta{
							Kind:       "ConfigMap",
							APIVersion: corev1.SchemeGroupVersion.String(),
						},
						ObjectMeta: metav1.ObjectMeta{
							Namespace: integration.Namespace,
							Name:      conf.Name(),
						},
					}
					configmaps = append(configmaps, kubernetes.LookupResourceVersion(ctx, client, &cm))
				} else if conf.StorageType() == utilResource.StorageTypeSecret {
					sec := corev1.Secret{
						TypeMeta: metav1.TypeMeta{
							Kind:       "Secret",
							APIVersion: corev1.SchemeGroupVersion.String(),
						},
						ObjectMeta: metav1.ObjectMeta{
							Namespace: integration.Namespace,
							Name:      conf.Name(),
						},
					}
					secrets = append(secrets, kubernetes.LookupResourceVersion(ctx, client, &sec))
				}
			}
		}
	}
	return secrets, configmaps
}

type controller interface {
	checkReadyCondition(ctx context.Context) (bool, error)
	getPodSpec() corev1.PodSpec
	updateReadyCondition(readyPods int) bool
	hasTemplateIntegrationLabel() bool
	getControllerName() string
}

func (action *monitorAction) newController(env *trait.Environment, integration *v1.Integration) (controller, error) {
	var controller controller
	var obj ctrl.Object
	switch {
	case integration.IsConditionTrue(v1.IntegrationConditionDeploymentAvailable):
		obj = getUpdatedController(env, &appsv1.Deployment{})
		deploy, ok := obj.(*appsv1.Deployment)
		if !ok {
			return nil, fmt.Errorf("type assertion failed, not a Deployment: %v", obj)
		}
		controller = &deploymentController{
			obj:         deploy,
			integration: integration,
		}
	case integration.IsConditionTrue(v1.IntegrationConditionKnativeServiceAvailable):
		obj = getUpdatedController(env, &servingv1.Service{})
		svc, ok := obj.(*servingv1.Service)
		if !ok {
			return nil, fmt.Errorf("type assertion failed, not a Knative Service: %v", obj)
		}
		controller = &knativeServiceController{
			obj:         svc,
			integration: integration,
		}
	case integration.IsConditionTrue(v1.IntegrationConditionCronJobAvailable):
		obj = getUpdatedController(env, &batchv1.CronJob{})
		cj, ok := obj.(*batchv1.CronJob)
		if !ok {
			return nil, fmt.Errorf("type assertion failed, not a CronJob: %v", obj)
		}
		controller = &cronJobController{
			obj:         cj,
			integration: integration,
			client:      action.client,
		}
	default:
		return nil, fmt.Errorf("unsupported controller for integration %s", integration.Name)
	}

	if obj == nil {
		return nil, fmt.Errorf("unable to retrieve controller for integration %s", integration.Name)
	}

	return controller, nil
}

// getUpdatedController retrieves the controller updated from the deployer trait execution.
func getUpdatedController(env *trait.Environment, obj ctrl.Object) ctrl.Object {
	return env.Resources.GetController(func(object ctrl.Object) bool {
		return reflect.TypeOf(obj) == reflect.TypeOf(object)
	})
}

func (action *monitorAction) updateIntegrationPhaseAndReadyCondition(
	ctx context.Context, controller controller, environment *trait.Environment, integration *v1.Integration,
	pendingPods []corev1.Pod, runningPods []corev1.Pod,
) error {
	if done, err := controller.checkReadyCondition(ctx); done || err != nil {
		// There may be pods that are not ready but still probable for getting error messages.
		// Ignore returned error from probing as it's expected when the ctrl obj is not ready.
		_, _, _ = action.probeReadiness(ctx, environment, integration, runningPods)
		return err
	}
	if arePodsFailingStatuses(integration, pendingPods, runningPods) {
		return nil
	}
	readyPods, probeOk, err := action.probeReadiness(ctx, environment, integration, runningPods)
	if err != nil {
		return err
	}
	if !probeOk {
		integration.Status.Phase = v1.IntegrationPhaseError
		return nil
	}
	if done := controller.updateReadyCondition(readyPods); done {
		integration.Status.Phase = v1.IntegrationPhaseRunning
		return nil
	}

	return nil
}

func arePodsFailingStatuses(integration *v1.Integration, pendingPods []corev1.Pod, runningPods []corev1.Pod) bool {
	// Check Pods statuses
	for _, pod := range pendingPods {
		// Check the scheduled condition
		if scheduled := kubernetes.GetPodCondition(pod, corev1.PodScheduled); scheduled != nil &&
			scheduled.Status == corev1.ConditionFalse &&
			scheduled.Reason == "Unschedulable" {
			integration.Status.Phase = v1.IntegrationPhaseError
			integration.SetReadyConditionError(scheduled.Message)
			return true
		}
	}
	// Check pending container statuses
	for _, pod := range pendingPods {
		var containers []corev1.ContainerStatus
		containers = append(containers, pod.Status.InitContainerStatuses...)
		containers = append(containers, pod.Status.ContainerStatuses...)
		for _, container := range containers {
			// Check the images are pulled
			if waiting := container.State.Waiting; waiting != nil && waiting.Reason == "ImagePullBackOff" {
				integration.Status.Phase = v1.IntegrationPhaseError
				integration.SetReadyConditionError(waiting.Message)
				return true
			}
		}
	}
	// Check running container statuses
	for _, pod := range runningPods {
		if pod.DeletionTimestamp != nil {
			continue
		}
		var containers []corev1.ContainerStatus
		containers = append(containers, pod.Status.InitContainerStatuses...)
		containers = append(containers, pod.Status.ContainerStatuses...)
		for _, container := range containers {
			// Check the container state
			if waiting := container.State.Waiting; waiting != nil && waiting.Reason == "CrashLoopBackOff" {
				integration.Status.Phase = v1.IntegrationPhaseError
				integration.SetReadyConditionError(waiting.Message)
				return true
			}
			if terminated := container.State.Terminated; terminated != nil && terminated.Reason == "Error" {
				integration.Status.Phase = v1.IntegrationPhaseError
				integration.SetReadyConditionError(terminated.Message)
				return true
			}
		}
	}

	return false
}

// probeReadiness calls the readiness probes of the non-ready Pods directly to retrieve insights from the Camel runtime.
// The func return the number of readyPods, the success of the probe and any error may have happened during its execution.
func (action *monitorAction) probeReadiness(ctx context.Context, environment *trait.Environment, integration *v1.Integration, pods []corev1.Pod) (int, bool, error) {
	// as a default we assume the Integration is Ready
	readyCondition := v1.IntegrationCondition{
		Type:   v1.IntegrationConditionReady,
		Status: corev1.ConditionTrue,
		Pods:   make([]v1.PodCondition, len(pods)),
	}

	readyPods := 0
	unreadyPods := 0

	runtimeReady := true
	runtimeFailed := false
	probeReadinessOk := true

	for i := range pods {
		pod := &pods[i]
		readyCondition.Pods[i].Name = pod.Name
		for p := range pod.Status.Conditions {
			if pod.Status.Conditions[p].Type == corev1.PodReady {
				readyCondition.Pods[i].Condition = pod.Status.Conditions[p]
				break
			}
		}
		// If it's in ready status, then we don't care to probe.
		if ready := kubernetes.GetPodCondition(*pod, corev1.PodReady); ready.Status == corev1.ConditionTrue {
			readyPods++
			continue
		}
		unreadyPods++
		container := getIntegrationContainer(environment, pod)
		if container == nil {
			return readyPods, false, fmt.Errorf("integration container not found in Pod %s/%s", pod.Namespace, pod.Name)
		}
		if probe := container.ReadinessProbe; probe != nil && probe.HTTPGet != nil {
			body, err := proxyGetHTTPProbe(ctx, action.client, probe, pod, container)
			// When invoking the HTTP probe, the kubernetes client exposes a very
			// specific behavior:
			//
			// - if there is no error, that means the pod in not ready just because
			//   the probe has to be called few time as per configuration, so it means
			//   it's not ready, but the probe is OK, and the pod could become ready
			//   at some point
			// - if the error is Service Unavailable (HTTP 503) then it means the pod
			//   is not ready and the probe is failing, in this case we can use the
			//   response to scrape for camel info
			//
			// Here an example of a failed probe (from curl):
			//
			//   Trying 127.0.0.1:8080...
			//   TCP_NODELAY set
			//   Connected to localhost (127.0.0.1) port 8080 (#0)
			//   GET /q/health/ready HTTP/1.1
			//   Host: localhost:8080
			//   User-Agent: curl/7.68.0
			//   Accept: */*
			//
			//   Mark bundle as not supporting multiuse
			//   HTTP/1.1 503 Service Unavailable
			//   content-type: application/json; charset=UTF-8
			//   content-length: 871
			//
			//   {
			//     "status": "DOWN",
			//     "checks": [ {
			//       "name": "camel-routes",
			//       "status": "DOWN",
			//       "data": {
			//         "route.id": "route1",
			//         "route.status": "Stopped",
			//         "check.kind": "READINESS"
			//       }
			//     }]
			//   }
			if err == nil {
				continue
			}

			if errors.Is(err, context.DeadlineExceeded) {
				readyCondition.Pods[i].Condition.Message = fmt.Sprintf("readiness probe timed out for Pod %s/%s", pod.Namespace, pod.Name)
				runtimeReady = false
				continue
			}
			if !k8serrors.IsServiceUnavailable(err) {
				readyCondition.Pods[i].Condition.Message = fmt.Sprintf("readiness probe failed for Pod %s/%s: %s", pod.Namespace, pod.Name, err.Error())
				runtimeReady = false
				continue
			}

			health, err := NewHealthCheck(body)
			if err != nil {
				return readyPods, false, err
			}
			for _, check := range health.Checks {
				if check.Status == v1.HealthCheckStatusUp {
					continue
				}

				runtimeReady = false
				runtimeFailed = true

				readyCondition.Pods[i].Health = append(readyCondition.Pods[i].Health, check)
			}
		}
	}

	if runtimeFailed {
		probeReadinessOk = false
		readyCondition.Reason = v1.IntegrationConditionErrorReason
		readyCondition.Status = corev1.ConditionFalse
		readyCondition.Message = fmt.Sprintf("%d/%d pods are not ready", unreadyPods, unreadyPods+readyPods)
		integration.Status.SetConditions(readyCondition)
	}
	if !runtimeReady {
		probeReadinessOk = false
		readyCondition.Reason = v1.IntegrationConditionRuntimeNotReadyReason
		readyCondition.Status = corev1.ConditionFalse
		readyCondition.Message = fmt.Sprintf("%d/%d pods are not ready", unreadyPods, unreadyPods+readyPods)
		integration.Status.SetConditions(readyCondition)
	}

	return readyPods, probeReadinessOk, nil
}

func findHighestPriorityReadyKit(kits []v1.IntegrationKit) (*v1.IntegrationKit, error) {
	if len(kits) == 0 {
		return nil, nil
	}
	var kit *v1.IntegrationKit
	priority := 0
	for i, k := range kits {
		if k.Status.Phase != v1.IntegrationKitPhaseReady {
			continue
		}
		p, err := strconv.Atoi(k.Labels[v1.IntegrationKitPriorityLabel])
		if err != nil {
			return nil, err
		}
		if p > priority {
			kit = &kits[i]
			priority = p
		}
	}
	return kit, nil
}

func getIntegrationContainer(environment *trait.Environment, pod *corev1.Pod) *corev1.Container {
	name := environment.GetIntegrationContainerName()
	for i, container := range pod.Spec.Containers {
		if container.Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}
