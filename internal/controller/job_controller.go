/*
Copyright 2024.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	apps "k8s.io/api/apps/v1"
	batch "k8s.io/api/batch/v1"
	core "k8s.io/api/core/v1"
	net "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/andrescosta/goico/pkg/collection"
	"github.com/andrescosta/goico/pkg/ref"
	jobicov1 "github.com/andrescosta/jobicok8s/api/v1"
)

// JobReconciler reconciles a Job object
type JobReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type ids struct {
	Service    string
	Deployment string
	Job        string
	Ingress    string
}

func newIds(e jobicov1.Event, jobdef jobicov1.Job) ids {
	return ids{
		Service:    fmt.Sprintf("%s-service-%s", jobdef.Name, e.Name),
		Deployment: fmt.Sprintf("%s-deployment-%s", jobdef.Name, e.Name),
		Job:        fmt.Sprintf("%s-exec-%s", jobdef.Name, e.Name),
		Ingress:    fmt.Sprintf("%s-ingress-%s", jobdef.Name, e.Name),
	}
}

// +kubebuilder:rbac:groups=jobico.coeux.dev,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=jobico.coeux.dev,resources=jobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=jobico.coeux.dev,resources=jobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;update;patch;delete;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=create;update;patch;delete;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=create;update;patch;delete;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=create;update;patch;delete;list;watch

func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var jobdef jobicov1.Job
	if err := r.Get(ctx, req.NamespacedName, &jobdef); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger.Info("creating dependencies v2", "job", jobdef.Name)
	requeue := false
	for _, evt := range jobdef.Spec.Events {
		i := newIds(evt, jobdef)

		// Deployment
		if err := r.reconcileDeployment(ctx, i, evt, jobdef); err != nil {
			logger.Error(err, "Error while reconciling Deployments", "job", jobdef.Name)
			return ctrl.Result{}, err
		}

		// Service
		if err := r.reconcileService(ctx, i, evt, jobdef); err != nil {
			logger.Error(err, "Error while reconciling Services", "job", jobdef.Name)
			return ctrl.Result{}, err
		}

		// Ingress
		if err := r.reconcileIngress(ctx, i, evt, jobdef); err != nil {
			logger.Error(err, "Error while reconciling Ingresses", "job", jobdef.Name)
			return ctrl.Result{}, err
		}

		// Job
		if len(evt.Wasm) > 0 {
			var err error
			requeue, err = r.reconcileJob(ctx, i, evt, jobdef)
			if err != nil {
				logger.Error(err, "Error while reconciling Services", "job", jobdef.Name)
				return ctrl.Result{}, err
			}
		}
		if len(evt.Script) > 0 {
			var err error
			requeue, err = r.reconcileJobInt(ctx, i, evt, jobdef)
			if err != nil {
				logger.Error(err, "Error while reconciling Services", "job", jobdef.Name)
				return ctrl.Result{}, err
			}
		}
	}
	err := r.garbageCollect(ctx, jobdef)
	if err != nil {
		logger.Error(err, "Error during garbage collector phase.")
		return ctrl.Result{}, err
	}
	return ctrl.Result{
		Requeue: requeue,
	}, nil
}

func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Owns(&core.Service{}).
		Owns(&net.Ingress{}).
		Owns(&batch.Job{}).
		Owns(&apps.Deployment{}).
		For(&jobicov1.Job{}).
		Complete(r)
}

func (r *JobReconciler) reconcileJob(ctx context.Context, i ids, evt jobicov1.Event, jobdef jobicov1.Job) (bool, error) {
	current, err := r.jobDefinition(i.Job, jobdef, evt)
	if err != nil {
		return false, err
	}
	orig := new(batch.Job)
	exist, err := r.get(ctx, orig, i.Job, jobdef.Namespace)
	if err != nil {
		return false, err
	}
	if exist {
		if !orig.DeletionTimestamp.IsZero() {
			return true, nil
		}
		if reflect.DeepEqual(
			current.Spec.Template.Spec.Containers[0].Env,
			orig.Spec.Template.Spec.Containers[0].Env) {
			return false, nil
		}

		// the Job's template section is immutable and cannot be updated.
		// We delete it and recreate later.
		err := r.Delete(ctx, orig, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)})
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if err := r.Create(ctx, current); err != nil {
		return false, err
	}
	return false, nil
}

func (r *JobReconciler) reconcileJobInt(ctx context.Context, i ids, evt jobicov1.Event, jobdef jobicov1.Job) (bool, error) {
	current, err := r.jobDefinitionInt(i.Job, jobdef, evt)
	if err != nil {
		return false, err
	}
	orig := new(batch.Job)
	exist, err := r.get(ctx, orig, i.Job, jobdef.Namespace)
	if err != nil {
		return false, err
	}
	if exist {
		if !orig.DeletionTimestamp.IsZero() {
			return true, nil
		}
		if reflect.DeepEqual(
			current.Spec.Template.Spec.Containers[0].Env,
			orig.Spec.Template.Spec.Containers[0].Env) {
			return false, nil
		}

		// the Job's template section is immutable and cannot be updated.
		// We delete it and recreate later.
		err := r.Delete(ctx, orig, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)})
		if err != nil {
			return false, err
		}
		return true, nil
	}
	if err := r.Create(ctx, current); err != nil {
		return false, err
	}
	return false, nil
}

func (r *JobReconciler) reconcileIngress(ctx context.Context, i ids, evt jobicov1.Event, jobdef jobicov1.Job) error {
	orig := new(net.Ingress)
	current, err := r.ingressDefinition(i, jobdef, evt)
	if err != nil {
		return err
	}
	exist, err := r.get(ctx, orig, i.Ingress, jobdef.Namespace)
	if err != nil {
		return err
	}
	if exist {
		if current.ObjectMeta.Annotations["event"] != orig.ObjectMeta.Annotations["event"] {
			return r.Patch(ctx, current, client.StrategicMergeFrom(orig))
		}
		return nil
	}
	if err := r.Create(ctx, current); err != nil {
		return err
	}
	return nil
}

func (r *JobReconciler) reconcileService(ctx context.Context, i ids, evt jobicov1.Event, jobdef jobicov1.Job) error {
	current, err := r.serviceDefinition(i.Service, jobdef, evt)
	if err != nil {
		return err
	}
	orig := new(core.Service)
	exist, err := r.get(ctx, orig, i.Service, jobdef.Namespace)
	if err != nil {
		return err
	}
	if exist {
		if current.Labels["event"] != orig.Labels["event"] {
			return r.Patch(ctx, orig, client.StrategicMergeFrom(current))
		}
		return nil
	}
	if err := r.Create(ctx, current); err != nil {
		return err
	}
	return nil
}

func (r *JobReconciler) reconcileDeployment(ctx context.Context, i ids, evt jobicov1.Event, jobdef jobicov1.Job) error {
	orig := new(apps.Deployment)
	current, err := r.deploymentDefinition(i.Deployment, jobdef, evt)
	if err != nil {
		return err
	}
	exist, err := r.get(ctx, orig, i.Deployment, jobdef.Namespace)
	if err != nil {
		return err
	}
	if exist {
		if !reflect.DeepEqual(orig.Spec.Template.Spec.Containers[0].Env, orig.Spec.Template.Spec.Containers[0].Env) {
			return r.Patch(ctx, current, client.StrategicMergeFrom(orig))
		}
		return nil
	}
	if err := r.Create(ctx, current); err != nil {
		return err
	}
	return nil
}

func (r *JobReconciler) garbageCollect(ctx context.Context, jobdef jobicov1.Job) error {
	var err error
	evs := collection.JoinOf(jobdef.Spec.Events, ",", func(e jobicov1.Event) string { return e.Name })
	expr := fmt.Sprintf("owner=%s, event notin(%s)", jobdef.Name, evs)
	labelSelector, err := labels.Parse(expr)
	if err != nil {
		return err
	}
	opts := &client.ListOptions{LabelSelector: labelSelector}
	igs := net.IngressList{}
	if err := r.List(ctx, &igs, opts); err != nil {
		return err
	}
	for _, i := range igs.Items {
		err = errors.Join(r.Delete(ctx, &i, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)}), err)
	}

	svcs := core.ServiceList{}
	if err := r.List(ctx, &svcs, opts); err != nil {
		return err
	}
	for _, s := range svcs.Items {
		err = errors.Join(r.Delete(ctx, &s, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)}), err)
	}

	dpls := apps.DeploymentList{}
	if err := r.List(ctx, &dpls, opts); err != nil {
		return err
	}
	for _, d := range dpls.Items {
		err = errors.Join(r.Delete(ctx, &d, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)}), err)
	}

	objs := batch.JobList{}
	if err := r.List(ctx, &objs, opts); err != nil {
		return err
	}
	for _, o := range objs.Items {
		err = errors.Join(r.Delete(ctx, &o, &client.DeleteOptions{PropagationPolicy: ref.Of(v1.DeletePropagationBackground)}), err)
	}
	return nil
}

func (r *JobReconciler) ingressDefinition(i ids, jobdef jobicov1.Job, e jobicov1.Event) (*net.Ingress, error) {
	ingress := net.Ingress{
		ObjectMeta: v1.ObjectMeta{
			Labels:    map[string]string{"owner": jobdef.Name, "event": e.Name},
			Name:      i.Ingress,
			Namespace: jobdef.Namespace,
			Annotations: map[string]string{
			//	"nginx.ingress.kubernetes.io/ssl-redirect": "true",
				"event": e.Name,
			},
		},
		Spec: net.IngressSpec{
			//IngressClassName: ref.Of("nginx"),
			Rules: []net.IngressRule{
				{
					Host: "listener.jobico.org",
					IngressRuleValue: net.IngressRuleValue{
						HTTP: &net.HTTPIngressRuleValue{
							Paths: []net.HTTPIngressPath{
								{
									Path:     "/listener/" + e.Name,
									PathType: ref.Of(net.PathType("Prefix")),
									Backend: net.IngressBackend{
										Service: &net.IngressServiceBackend{
											Name: i.Service,
											Port: net.ServiceBackendPort{
												Number: 8080,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(&jobdef, &ingress, r.Scheme); err != nil {
		return nil, err
	}
	return &ingress, nil
}

func (r *JobReconciler) serviceDefinition(serviceName string, jobdef jobicov1.Job, e jobicov1.Event) (*core.Service, error) {
	service := core.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:      serviceName,
			Namespace: jobdef.Namespace,
			Labels:    map[string]string{"app": "listener", "event": e.Name, "owner": jobdef.Name},
		},
		Spec: core.ServiceSpec{
			Selector: map[string]string{"app": "listener", "event": e.Name},
			Ports:    []core.ServicePort{{Port: 8080, TargetPort: intstr.FromInt(8080)}},
            Type:     core.ServiceTypeLoadBalancer,
		},
	}
	if err := ctrl.SetControllerReference(&jobdef, &service, r.Scheme); err != nil {
		return nil, err
	}
	return &service, nil
}

func (r *JobReconciler) deploymentDefinition(deploymentName string, jobdef jobicov1.Job, e jobicov1.Event) (*apps.Deployment, error) {
	deployment := apps.Deployment{
		ObjectMeta: v1.ObjectMeta{
			Labels:    map[string]string{"owner": jobdef.Name, "event": e.Name},
			Name:      deploymentName,
			Namespace: jobdef.Namespace,
		},
		Spec: apps.DeploymentSpec{
			Replicas: ref.Of(int32(1)),
			Selector: &v1.LabelSelector{
				MatchLabels: map[string]string{"app": "listener", "event": e.Name},
			},
			Template: core.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: map[string]string{"app": "listener", "event": e.Name},
				},
				Spec: core.PodSpec{
                    ImagePullSecrets: []core.LocalObjectReference{
                        {
                            Name: "reg-cred-secret",
                        },
                    },
					RestartPolicy: core.RestartPolicyAlways,
					Volumes: []core.Volume{
						{
							Name: "schema",
							VolumeSource: core.VolumeSource{
								ConfigMap: &core.ConfigMapVolumeSource{
									DefaultMode: ref.Of(int32(420)),
									LocalObjectReference: core.LocalObjectReference{
										Name: e.Schema.Key,
									},
								},
							},
						},
					},
					Containers: []core.Container{
						{
							Name:            "listener-" + e.Name,
							Image:           "reg.jobico.org/listener:v1",
							//ImagePullPolicy: core.PullNever,
							Ports: []core.ContainerPort{
								{
									ContainerPort: 8080,
								},
							},
							Env: []core.EnvVar{
								{
									Name:  "event",
									Value: e.Name,
								},
								{
									Name:  "NATS_URL",
									Value: "nats://nats:4222",
								},
								{
									Name:  "schema",
									Value: e.Schema.Key + ".json",
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "schema",
									MountPath: "/etc/listener",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(&jobdef, &deployment, r.Scheme); err != nil {
		return nil, err
	}
	return &deployment, nil
}

func (r *JobReconciler) jobDefinition(jobName string, jobdef jobicov1.Job, evt jobicov1.Event) (*batch.Job, error) {
	job := batch.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:      jobName,
			Namespace: jobdef.Namespace,
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: map[string]string{"app": "exec", "event": evt.Name, "owner": jobdef.Name},
				},
				Spec: core.PodSpec{
                    ImagePullSecrets: []core.LocalObjectReference{
                        {
                            Name: "reg-cred-secret",
                        },
                    },
					RestartPolicy: core.RestartPolicyOnFailure,
					Volumes: []core.Volume{
						{
							Name: "local-persistent-storage",
							VolumeSource: core.VolumeSource{
								PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
									ClaimName: "test-pvc",
								},
							},
						},
					},
					Containers: []core.Container{
						{
							Name:            "exec-" + evt.Name,
							Image:           "reg.jobico.org/exec:v1",
                            //ImagePullPolicy: core.PullNever,
							Env: []core.EnvVar{
								{
									Name:  "event",
									Value: evt.Name,
								},
								{
									Name:  "NATS_URL",
									Value: "nats://nats:4222",
								},
								{
									Name:  "wasm",
									Value: evt.Wasm,
								},
								{
									Name:  "dir",
									Value: "/mnt/exec",
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "local-persistent-storage",
									MountPath: "/mnt/exec",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(&jobdef, &job, r.Scheme); err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *JobReconciler) jobDefinitionInt(jobName string, jobdef jobicov1.Job, evt jobicov1.Event) (*batch.Job, error) {
	job := batch.Job{
		ObjectMeta: v1.ObjectMeta{
			Name:      jobName,
			Namespace: jobdef.Namespace,
		},
		Spec: batch.JobSpec{
			Template: core.PodTemplateSpec{
				ObjectMeta: v1.ObjectMeta{
					Labels: map[string]string{"app": "exec", "event": evt.Name, "owner": jobdef.Name},
				},
				Spec: core.PodSpec{
                    ImagePullSecrets: []core.LocalObjectReference{
                        {
                            Name: "reg-cred-secret",
                        },
                    },
					RestartPolicy: core.RestartPolicyOnFailure,
					Volumes: []core.Volume{
						{
							Name: "local-persistent-storage",
							VolumeSource: core.VolumeSource{
								PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{
									ClaimName: "test-pvc",
								},
							},
						},
					},
					Containers: []core.Container{
						{
							Name:            "exec-" + evt.Name,
							Image:           "reg.jobico.org/exec:v1",
                            //ImagePullPolicy: core.PullNever,
							Env: []core.EnvVar{
								{
									Name:  "event",
									Value: evt.Name,
								},
								{
									Name:  "NATS_URL",
									Value: "nats://nats:4222",
								},
								{
									Name:  "script",
									Value: evt.Script,
								},
								{
									Name:  "dir",
									Value: "/mnt/exec",
								},
							},
							VolumeMounts: []core.VolumeMount{
								{
									Name:      "local-persistent-storage",
									MountPath: "/mnt/exec",
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(&jobdef, &job, r.Scheme); err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *JobReconciler) get(ctx context.Context, o client.Object, name, namespace string) (bool, error) {
	nn := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}
	err := r.Get(ctx, nn, o)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// object does not exist
			return false, nil
		}
		// error
		return false, err
	}
	// object exist
	return true, nil
}
