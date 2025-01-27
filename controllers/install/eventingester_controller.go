/*
Copyright 2022.

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

package install

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	installv1alpha1 "github.com/armadaproject/armada-operator/apis/install/v1alpha1"
	"github.com/armadaproject/armada-operator/controllers/builders"
)

// EventIngesterReconciler reconciles a EventIngester object
type EventIngesterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=install.armadaproject.io,resources=eventingesters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=install.armadaproject.io,resources=eventingesters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core,resources=secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *EventIngesterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("namespace", req.Namespace, "name", req.Name)
	started := time.Now()
	logger.Info("Reconciling EventIngester object")

	logger.Info("Fetching EventIngester object from cache")
	var eventIngester installv1alpha1.EventIngester
	if err := r.Client.Get(ctx, req.NamespacedName, &eventIngester); err != nil {
		if k8serrors.IsNotFound(err) {
			logger.Info("EventIngester not found in cache, ending reconcile")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	pc, err := installv1alpha1.BuildPortConfig(eventIngester.Spec.ApplicationConfig)
	if err != nil {
		return ctrl.Result{}, err
	}
	eventIngester.Spec.PortConfig = pc

	components, err := r.generateEventIngesterComponents(&eventIngester, r.Scheme)
	if err != nil {
		return ctrl.Result{}, err
	}

	deletionTimestamp := eventIngester.ObjectMeta.DeletionTimestamp
	// examine DeletionTimestamp to determine if object is under deletion
	if !deletionTimestamp.IsZero() {
		logger.Info("EventIngester is being deleted")

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	componentsCopy := components.DeepCopy()

	mutateFn := func() error {
		components.ReconcileComponents(componentsCopy)
		return nil
	}

	if components.ServiceAccount != nil {
		logger.Info("Upserting EventIngester ServiceAccount object")
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, components.ServiceAccount, mutateFn); err != nil {
			return ctrl.Result{}, err
		}
	}

	if components.Secret != nil {
		logger.Info("Upserting EventIngester Secret object")
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, components.Secret, mutateFn); err != nil {
			return ctrl.Result{}, err
		}
	}

	if components.Deployment != nil {
		logger.Info("Upserting EventIngester Deployment object")
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, components.Deployment, mutateFn); err != nil {
			return ctrl.Result{}, err
		}
	}

	logger.Info("Successfully reconciled EventIngester object", "durationMilis", time.Since(started).Milliseconds())

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *EventIngesterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&installv1alpha1.EventIngester{}).
		Complete(r)
}

func (r *EventIngesterReconciler) generateEventIngesterComponents(eventIngester *installv1alpha1.EventIngester, scheme *runtime.Scheme) (*CommonComponents, error) {
	secret, err := builders.CreateSecret(eventIngester.Spec.ApplicationConfig, eventIngester.Name, eventIngester.Namespace, GetConfigFilename(eventIngester.Name))
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetOwnerReference(eventIngester, secret, scheme); err != nil {
		return nil, err
	}
	deployment, err := r.createDeployment(eventIngester)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetOwnerReference(eventIngester, deployment, scheme); err != nil {
		return nil, err
	}

	serviceAccount := builders.CreateServiceAccount(eventIngester.Name, eventIngester.Namespace, AllLabels(eventIngester.Name, eventIngester.Labels), eventIngester.Spec.ServiceAccount)
	if err := controllerutil.SetOwnerReference(eventIngester, serviceAccount, scheme); err != nil {
		return nil, err
	}
	return &CommonComponents{
		Deployment:     deployment,
		ServiceAccount: serviceAccount,
		Secret:         secret,
	}, nil
}

func (r *EventIngesterReconciler) createDeployment(eventIngester *installv1alpha1.EventIngester) (*appsv1.Deployment, error) {
	var runAsUser int64 = 1000
	var runAsGroup int64 = 2000
	allowPrivilegeEscalation := false
	env := createEnv(eventIngester.Spec.Environment)
	pulsarConfig, err := ExtractPulsarConfig(eventIngester.Spec.ApplicationConfig)
	if err != nil {
		return nil, err
	}
	volumes := createVolumes(eventIngester.Name, eventIngester.Spec.AdditionalVolumes)
	volumes = append(volumes, createPulsarVolumes(pulsarConfig)...)
	volumeMounts := createVolumeMounts(GetConfigFilename(eventIngester.Name), eventIngester.Spec.AdditionalVolumeMounts)
	volumeMounts = append(volumeMounts, createPulsarVolumeMounts(pulsarConfig)...)

	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: eventIngester.Name, Namespace: eventIngester.Namespace, Labels: AllLabels(eventIngester.Name, eventIngester.Labels)},
		Spec: appsv1.DeploymentSpec{
			Replicas: &eventIngester.Spec.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: IdentityLabel(eventIngester.Name),
			},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &intstr.IntOrString{IntVal: int32(1)},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:        eventIngester.Name,
					Namespace:   eventIngester.Namespace,
					Labels:      AllLabels(eventIngester.Name, eventIngester.Labels),
					Annotations: map[string]string{"checksum/config": GenerateChecksumConfig(eventIngester.Spec.ApplicationConfig.Raw)},
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: eventIngester.Spec.TerminationGracePeriodSeconds,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  &runAsUser,
						RunAsGroup: &runAsGroup,
					},
					Affinity: &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
								Weight: 100,
								PodAffinityTerm: corev1.PodAffinityTerm{
									TopologyKey: "kubernetes.io/hostname",
									LabelSelector: &metav1.LabelSelector{
										MatchExpressions: []metav1.LabelSelectorRequirement{{
											Key:      "app",
											Operator: metav1.LabelSelectorOpIn,
											Values:   []string{eventIngester.Name},
										}},
									},
								},
							}},
						},
					},
					Containers: []corev1.Container{{
						Name:            "eventingester",
						ImagePullPolicy: "IfNotPresent",
						Image:           ImageString(eventIngester.Spec.Image),
						Args:            []string{"--config", "/config/application_config.yaml"},
						Ports: []corev1.ContainerPort{{
							Name:          "metrics",
							ContainerPort: eventIngester.Spec.PortConfig.MetricsPort,
							Protocol:      "TCP",
						}},
						Env:             env,
						VolumeMounts:    volumeMounts,
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivilegeEscalation},
					}},
					NodeSelector: eventIngester.Spec.NodeSelector,
					Tolerations:  eventIngester.Spec.Tolerations,
					Volumes:      volumes,
				},
			},
		},
	}
	if eventIngester.Spec.Resources != nil {
		deployment.Spec.Template.Spec.Containers[0].Resources = *eventIngester.Spec.Resources
		deployment.Spec.Template.Spec.Containers[0].Env = addGoMemLimit(deployment.Spec.Template.Spec.Containers[0].Env, *eventIngester.Spec.Resources)
	}

	return &deployment, nil

}
