/*
Copyright 2019 tommylikehu@gmail.com.

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

package controllers

import (
	"context"
	errrorlib "errors"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/opensourceways/code-server-operator/controllers/initplugins"
	"github.com/opensourceways/code-server-operator/controllers/initplugins/interface"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	extv1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	resourcev1 "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"
	"strings"
	"time"

	csv1alpha1 "github.com/opensourceways/code-server-operator/api/v1alpha1"
)

const (
	CSNAME           = "code-server"
	MaxActiveSeconds = 60 * 60 * 24
	MaxKeepSeconds   = 60 * 60 * 24 * 30
	CertFile         = "tls.crt"
	CertKey          = "tls.key"
	HttpPort         = 8080
	HttpsPort        = 8443
	IngressLimitKey  = "kubernetes.io/ingress-bandwidth"
	EgressLimitKey   = "kubernetes.io/egress-bandwidth"
	StorageEmptyDir  = "emptyDir"
	DefaultPrefix    = "terminal"
	InstanceEndpoint = "instanceEndpoint"
)

// CodeServerReconciler reconciles a CodeServer object
type CodeServerReconciler struct {
	client.Client
	Log     logr.Logger
	Scheme  *runtime.Scheme
	Options *CodeServerOption
	ReqCh   chan CodeServerRequest
}

// +kubebuilder:rbac:groups=cs.opensourceways.com,resources=codeservers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cs.opensourceways.com,resources=codeservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=,resources=secrets,verbs=get;list;watch
func (r *CodeServerReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()
	reqLogger := r.Log.WithValues("codeserver", req.NamespacedName)
	// Fetch the CodeServer instance
	codeServer := &csv1alpha1.CodeServer{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, codeServer)
	if err != nil {
		if errors.IsNotFound(err) {
			reqLogger.Info("CodeServer has been deleted. Trying to delete its related resources.")
			r.deleteFromInactiveWatch(req.NamespacedName)
			r.deleteFromRecycleWatch(req.NamespacedName)
			if err := r.deleteCodeServerResource(req.Name, req.Namespace, codeServer.Spec.StorageName,
				true); err != nil {
				return reconcile.Result{Requeue: true}, err
			}
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reqLogger.Error(err, "Failed to get CoderServer.")
		return reconcile.Result{}, err
	}

	//case1. code server now stays inactive, we will delete all resources except volume.
	//case2. code server will be directly deleted after RecycleAfterSeconds if.
	//       InactiveAfterSeconds are configured with zero.
	//case3. code server now stays recycled, we will delete all resources.
	//case4. reconcile the code server.
	if HasCondition(codeServer.Status, csv1alpha1.ServerInactive) && !HasCondition(codeServer.Status, csv1alpha1.ServerRecycled) {
		//remove it from watch list and add it to recycle watch
		r.deleteFromInactiveWatch(req.NamespacedName)
		inActiveCondition := GetCondition(codeServer.Status, csv1alpha1.ServerInactive)
		if (codeServer.Spec.RecycleAfterSeconds == nil) || *codeServer.Spec.RecycleAfterSeconds <= 0 || *codeServer.Spec.RecycleAfterSeconds >= MaxKeepSeconds {
			// we keep the instance within MaxKeepSeconds maximumly
			reqLogger.Info(fmt.Sprintf("Code server will be recycled after %d seconds.",
				MaxKeepSeconds))
			r.addToRecycleWatch(req.NamespacedName, MaxKeepSeconds, inActiveCondition.LastTransitionTime)
		} else {
			reqLogger.Info(fmt.Sprintf("Code server will be recycled after %d seconds.",
				*codeServer.Spec.RecycleAfterSeconds))
			r.addToRecycleWatch(req.NamespacedName, *codeServer.Spec.RecycleAfterSeconds, inActiveCondition.LastTransitionTime)
		}
		if err := r.deleteCodeServerResource(codeServer.Name, codeServer.Namespace, codeServer.Spec.StorageName,
			false); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
	} else if !HasCondition(codeServer.Status, csv1alpha1.ServerRecycled) &&
		*codeServer.Spec.InactiveAfterSeconds == 0 && HasCondition(codeServer.Status, csv1alpha1.ServerReady) {
		current := metav1.Time{
			Time: time.Now(),
		}
		if (codeServer.Spec.RecycleAfterSeconds == nil) || *codeServer.Spec.RecycleAfterSeconds <= 0 || *codeServer.Spec.RecycleAfterSeconds >= MaxKeepSeconds {
			// we keep the instance within MaxKeepSeconds maximumly
			reqLogger.Info(fmt.Sprintf("Code server will be recycled after %d seconds.",
				MaxKeepSeconds))
			r.addToRecycleWatch(req.NamespacedName, MaxKeepSeconds, current)
		} else {
			reqLogger.Info(fmt.Sprintf("Code server will be recycled after %d seconds.",
				*codeServer.Spec.RecycleAfterSeconds))
			r.addToRecycleWatch(req.NamespacedName, *codeServer.Spec.RecycleAfterSeconds, current)
		}
	} else if HasCondition(codeServer.Status, csv1alpha1.ServerRecycled) {
		//remove it from watch list
		r.deleteFromInactiveWatch(req.NamespacedName)
		r.deleteFromRecycleWatch(req.NamespacedName)
		if err := r.deleteCodeServerResource(codeServer.Name, codeServer.Namespace, codeServer.Spec.StorageName,
			true); err != nil {
			return reconcile.Result{Requeue: true}, err
		}
	} else {
		var failed error
		var service *corev1.Service
		var deployment *appsv1.Deployment
		var conditon csv1alpha1.ServerCondition
		// 0/5 check whether we need enable https
		tlsSecret := r.findLegalCertSecrets(codeServer)
		// 1/5: reconcile PVC
		if r.needDeployPVC(codeServer.Spec.StorageName) {
			_, failed = r.reconcileForPVC(codeServer)
		}
		// 2/5: reconcile service
		if failed == nil {
			service, failed = r.reconcileForService(codeServer, tlsSecret)
		}
		// 3/5:reconcile ingress
		if failed == nil {
			_, failed = r.reconcileForIngress(codeServer, tlsSecret)
		}
		// 4/5: reconcile deployment
		if failed == nil {
			deployment, failed = r.reconcileForDeployment(codeServer, tlsSecret)
		}
		// 5/5: update code server status
		if !HasCondition(codeServer.Status, csv1alpha1.ServerCreated) {
			createdCondition := NewStateCondition(csv1alpha1.ServerCreated,
				"code server has been accepted", map[string]string{})
			SetCondition(&codeServer.Status, createdCondition)
		}
		if failed == nil {
			conditon = NewStateCondition(csv1alpha1.ServerReady,
				"code server now available", map[string]string{})
			if !HasDeploymentCondition(deployment.Status, appsv1.DeploymentAvailable) {
				conditon.Status = corev1.ConditionFalse
				conditon.Reason = "waiting deployment to be available"
			} else {
				//add instance endpoint
				conditon.Message[InstanceEndpoint] = fmt.Sprintf("https://%s.%s/%s/ws",
					codeServer.Spec.Subdomain,
					r.Options.DomainName, DefaultPrefix)
				//add it to watch list
				var endPoint string
				// only port differs, since no matter tls is enabled or nor we both expose upstream via http
				if tlsSecret == nil {
					endPoint = fmt.Sprintf("http://%s:%d/%s", service.Spec.ClusterIP, HttpPort,
						strings.TrimLeft(codeServer.Spec.ConnectProbe, "/"))
				} else {
					endPoint = fmt.Sprintf("http://%s:%d/%s", service.Spec.ClusterIP, HttpsPort,
						strings.TrimLeft(codeServer.Spec.ConnectProbe, "/"))
				}

				if (codeServer.Spec.InactiveAfterSeconds == nil) || *codeServer.Spec.InactiveAfterSeconds < 0 || *codeServer.Spec.InactiveAfterSeconds >= MaxActiveSeconds {
					// we keep the instance within MaxActiveSeconds maximumly
					r.addToInactiveWatch(req.NamespacedName, MaxActiveSeconds, endPoint)
					reqLogger.Info(fmt.Sprintf("Code server will be disactived after %d non-connection.",
						MaxActiveSeconds))
				} else if *codeServer.Spec.InactiveAfterSeconds == 0 {
					// we will not watch this code server instance if inactive is set 0
					reqLogger.Info("Code server will never be disactived")
				} else {
					r.addToInactiveWatch(req.NamespacedName, *codeServer.Spec.InactiveAfterSeconds, endPoint)
					reqLogger.Info(fmt.Sprintf("Code server will be disactived after %d non-connection.",
						*codeServer.Spec.InactiveAfterSeconds))
				}

			}
		} else {
			conditon = NewStateCondition(csv1alpha1.ServerErrored,
				"code server errored", map[string]string{"detail": failed.Error()})
		}
		SetCondition(&codeServer.Status, conditon)
		updateStatus := codeServer.Status
		err = r.Client.Get(context.TODO(), req.NamespacedName, codeServer)
		if err != nil {
			reqLogger.Error(err, "Failed to get CoderServer object for update.")
			return reconcile.Result{Requeue: true}, err
		}
		codeServer.Status = updateStatus
		err = r.Client.Update(context.TODO(), codeServer)
		if err != nil {
			reqLogger.Error(err, "Failed to update code server status.")
			return reconcile.Result{Requeue: true}, nil
		}
		if failed != nil {
			return reconcile.Result{
				Requeue:      true,
				RequeueAfter: time.Second * 30}, failed
		}
	}
	return reconcile.Result{Requeue: false}, nil
}

func (r *CodeServerReconciler) addToInactiveWatch(resource types.NamespacedName, duration int64, endpoint string) {
	request := CodeServerRequest{
		resource: resource,
		duration: duration,
		operate:  AddInactiveWatch,
		endpoint: endpoint,
	}
	r.ReqCh <- request
}

func (r *CodeServerReconciler) deleteFromInactiveWatch(resource types.NamespacedName) {
	request := CodeServerRequest{
		resource: resource,
		operate:  DeleteInactiveWatch,
	}
	r.ReqCh <- request
}

func (r *CodeServerReconciler) findLegalCertSecrets(codeServer *csv1alpha1.CodeServer) *corev1.Secret {
	reqLogger := r.Log.WithValues("namespace", codeServer.Namespace, "name", codeServer.Name)
	tlsSecret := &corev1.Secret{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: r.Options.HttpsSecretName, Namespace: codeServer.Namespace}, tlsSecret)
	if err == nil {
		if _, ok := tlsSecret.Data[CertFile]; !ok {
			reqLogger.Info(fmt.Sprintf("could not found secret key %s in secret, skip enabling https", CertFile))
			return nil
		}
		if _, ok := tlsSecret.Data[CertKey]; !ok {
			reqLogger.Info(fmt.Sprintf("could not found secret key %s in secret, skip enabling https", CertKey))
			return nil
		}
		reqLogger.Info(fmt.Sprintf("found secret %s in cluster, do enabling https", r.Options.HttpsSecretName))
		return tlsSecret
	}
	reqLogger.Info(fmt.Sprintf("could not found secret %s in cluster, skip enabling https", r.Options.HttpsSecretName))
	return nil
}

func (r *CodeServerReconciler) addToRecycleWatch(resource types.NamespacedName, duration int64, inactivetime metav1.Time) {
	request := CodeServerRequest{
		resource:     resource,
		operate:      AddRecycleWatch,
		duration:     duration,
		inactiveTime: inactivetime,
	}
	r.ReqCh <- request
}

func (r *CodeServerReconciler) deleteFromRecycleWatch(resource types.NamespacedName) {
	request := CodeServerRequest{
		resource: resource,
		operate:  DeleteRecycleWatch,
	}
	r.ReqCh <- request
}

func (r *CodeServerReconciler) deleteCodeServerResource(name, namespace string, storageName string, includePVC bool) error {
	reqLogger := r.Log.WithValues("namespace", name, "name", namespace)
	reqLogger.Info("Deleting code server resources.")
	//delete ingress
	ing := &extv1.Ingress{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, ing)
	//error of getting object is ignored
	if err == nil {
		err = r.Client.Delete(context.TODO(), ing)
		if err != nil {
			return err
		}
		reqLogger.Info("ingress resource has been successfully deleted.")
	} else if !errors.IsNotFound(err) {
		reqLogger.Info(fmt.Sprintf("failed to get ingress resource for deletion: %v", err))
	}
	//delete service
	srv := &corev1.Service{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, srv)
	//error of getting object is ignored
	if err == nil {
		err = r.Client.Delete(context.TODO(), srv)
		if err != nil {
			return err
		}
		reqLogger.Info("service resource has been successfully deleted.")
	} else if !errors.IsNotFound(err) {
		reqLogger.Info(fmt.Sprintf("failed to get service resource for deletion: %v", err))
	}
	//delete deployment
	app := &appsv1.Deployment{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, app)
	//error of getting object is ignored
	if err == nil {
		err = r.Client.Delete(context.TODO(), app)
		if err != nil {
			return err
		}
		reqLogger.Info("development resource has been successfully deleted.")
	} else if !errors.IsNotFound(err) {
		reqLogger.Info(fmt.Sprintf("failed to get development resource for deletion: %v", err))
	}
	if includePVC && r.needDeployPVC(storageName) {
		//delete pvc
		pvc := &corev1.PersistentVolumeClaim{}
		err = r.Client.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: namespace}, pvc)
		//error of getting object is ignored
		if err == nil {
			err = r.Client.Delete(context.TODO(), pvc)
			if err != nil {
				return err
			}
			reqLogger.Info("PVC resource has been successfully deleted.")
		} else if !errors.IsNotFound(err) {
			reqLogger.Info(fmt.Sprintf("failed to get PVC resource for deletion: %v", err))
		}
	}
	return nil
}

func (r *CodeServerReconciler) reconcileForPVC(codeServer *csv1alpha1.CodeServer) (*corev1.PersistentVolumeClaim, error) {
	reqLogger := r.Log.WithValues("namespace", codeServer.Namespace, "name", codeServer.Name)
	reqLogger.Info("Reconciling persistent volume claim.")
	//reconcile pvc for code server
	newPvc, err := r.pvcForCodeServer(codeServer)
	if err != nil {
		reqLogger.Error(err, "Failed to create new PersistentVolumeClaim.")
		return nil, err
	}
	oldPvc := &corev1.PersistentVolumeClaim{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: codeServer.Name, Namespace: codeServer.Namespace}, oldPvc)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a PersistentVolumeClaim.")
		err = r.Client.Create(context.TODO(), newPvc)
		if err != nil {
			reqLogger.Error(err, "Failed to create PersistentVolumeClaim.")
			return nil, err
		}
		return newPvc, nil
	} else {
		if err != nil {
			//Reschedule the event
			reqLogger.Error(err, fmt.Sprintf("Failed to get PVC for %s.", codeServer.Name))
			return nil, err
		}
		if needUpdatePVC(oldPvc, newPvc) {
			reqLogger.Error(err, "Updating PersistentVolumeClaim is not supported.")
			return oldPvc, nil
		}
	}
	return oldPvc, nil
}

func (r *CodeServerReconciler) reconcileForDeployment(codeServer *csv1alpha1.CodeServer, secret *corev1.Secret) (*appsv1.Deployment, error) {
	reqLogger := r.Log.WithValues("namespace", codeServer.Namespace, "name", codeServer.Name)
	reqLogger.Info("Reconciling Deployment.")
	//reconcile pvc for code server
	newDev, err := r.getDeployment(codeServer, secret)
	if err != nil {
		reqLogger.Error(err, "Failed to generate Deployment.")
		return nil, err
	}
	oldDev := &appsv1.Deployment{}
	err = r.Client.Get(context.TODO(), types.NamespacedName{Name: codeServer.Name, Namespace: codeServer.Namespace}, oldDev)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a Deployment.")
		err = r.Client.Create(context.TODO(), newDev)
		if err != nil {
			reqLogger.Error(err, "Failed to create Deployment.")
			return nil, err
		}
	} else {
		if err != nil {
			//Reschedule the event
			reqLogger.Error(err, fmt.Sprintf("Failed to get Deployment for %s.", codeServer.Name))
			return nil, err
		}
		if needUpdateDeployment(oldDev, newDev) {
			oldDev.Spec = newDev.Spec
			reqLogger.Info("Updating a Development.")
			err = r.Client.Update(context.TODO(), oldDev)
			if err != nil {
				reqLogger.Error(err, "Failed to update Deployment.")
				return nil, err
			}
		}
	}
	return oldDev, nil
}

func (r *CodeServerReconciler) reconcileForIngress(codeServer *csv1alpha1.CodeServer, secret *corev1.Secret) (*extv1.Ingress, error) {
	reqLogger := r.Log.WithValues("namespace", codeServer.Namespace, "name", codeServer.Name)
	reqLogger.Info("Reconciling ingress.")
	//reconcile ingress for code server
	newIngress := r.ingressForCodeServer(codeServer, secret)
	oldIngress := &extv1.Ingress{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: codeServer.Name, Namespace: codeServer.Namespace}, oldIngress)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a Ingress.")
		err = r.Client.Create(context.TODO(), newIngress)
		if err != nil {
			reqLogger.Error(err, "Failed to create Ingress.")
			return nil, err
		}
		// if update is required
	} else {
		if err != nil {
			//Reschedule the event
			reqLogger.Error(err, fmt.Sprintf("Failed to get Ingress for %s.", codeServer.Name))
			return nil, err
		}
		if !equality.Semantic.DeepEqual(oldIngress.Spec, newIngress.Spec) {
			oldIngress.Spec = newIngress.Spec
			reqLogger.Info("Updating a Ingress.")
			err = r.Client.Update(context.TODO(), oldIngress)
			if err != nil {
				reqLogger.Error(err, "Failed to update Ingress.")
				return nil, err
			}
		}
	}
	return oldIngress, nil
}

func (r *CodeServerReconciler) reconcileForService(codeServer *csv1alpha1.CodeServer, secret *corev1.Secret) (*corev1.Service, error) {
	reqLogger := r.Log.WithValues("namespace", codeServer.Namespace, "name", codeServer.Name)
	reqLogger.Info("Reconciling service.")
	//reconcile service for code server
	newService := r.serviceForCodeServer(codeServer, secret)
	oldService := &corev1.Service{}
	err := r.Client.Get(context.TODO(), types.NamespacedName{Name: codeServer.Name, Namespace: codeServer.Namespace}, oldService)
	if err != nil && errors.IsNotFound(err) {
		reqLogger.Info("Creating a Service.")
		err = r.Client.Create(context.TODO(), newService)
		if err != nil {
			reqLogger.Error(err, "Failed to create Service.")
			return nil, err
		}
		// if update is required
	} else {
		if err != nil {
			//Reschedule the event
			reqLogger.Error(err, fmt.Sprintf("Failed to get Service for %s.", codeServer.Name))
			return nil, err
		}
		if needUpdateService(oldService, newService) {
			oldService.Spec = newService.Spec
			reqLogger.Info("Updating a Service.")
			err = r.Client.Update(context.TODO(), oldService)
			if err != nil {
				reqLogger.Error(err, "Failed to update Service.")
				return nil, err
			}
		}
	}
	return oldService, nil
}

func (r *CodeServerReconciler) addInitContainersForDeployment(m *csv1alpha1.CodeServer, baseDir, baseDirVolume string) []corev1.Container {
	var containers []corev1.Container
	if len(m.Spec.InitPlugins) == 0 {
		return containers
	}
	reqLogger := r.Log.WithValues("namespace", m.Namespace, "name", m.Name)
	clientSet := _interface.PluginClients{Client: r.Client}
	for p, arguments := range m.Spec.InitPlugins {
		plugin, err := initplugins.CreatePlugin(clientSet, p, arguments, baseDir)
		if err != nil {
			reqLogger.Error(err, fmt.Sprintf("Failed to initialize init plugin %s", p))
			continue
		}
		container := plugin.GenerateInitContainerSpec()
		//Add work space volume in default
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			MountPath: baseDir,
			Name:      baseDirVolume,
		})
		containers = append(containers, *container)
	}
	return containers
}

func (r *CodeServerReconciler) getDeployment(m *csv1alpha1.CodeServer, secret *corev1.Secret) (*appsv1.Deployment, error) {
	runtime := string(m.Spec.Runtime)
	if strings.EqualFold(runtime, string(csv1alpha1.RuntimeCode)) {
		return r.deploymentForCodeServer(m, secret), nil
	} else if strings.EqualFold(runtime, string(csv1alpha1.RuntimeGotty)) {
		return r.deploymentForGotty(m, secret), nil
	} else {
		return nil, errrorlib.New(fmt.Sprintf("unsupported runtime %s", m.Spec.Runtime))
	}
}

// deploymentForCodeServer returns a code server Deployment object
func (r *CodeServerReconciler) deploymentForCodeServer(m *csv1alpha1.CodeServer, secret *corev1.Secret) *appsv1.Deployment {
	reqLogger := r.Log.WithValues("namespace", m.Namespace, "name", m.Name)
	baseCodeDir := "/home/coder/project"
	baseCodeVolume := "code-server-project-dir"
	ls := labelsForCodeServer(m.Name)
	replicas := int32(1)
	enablePriviledge := m.Spec.Privileged
	priviledged := corev1.SecurityContext{
		Privileged: enablePriviledge,
	}

	dataVolume := corev1.PersistentVolumeClaimVolumeSource{
		ClaimName: m.Name,
	}
	arguments := []string{"--base-path", r.getInstanceUrl(m)}
	if secret == nil {
		arguments = append(arguments, []string{"--port", "8080"}...)
	} else {
		arguments = append(arguments, []string{"--port", "8443"}...)
	}

	initContainer := r.addInitContainersForDeployment(m, baseCodeDir, baseCodeVolume)
	reqLogger.Info(fmt.Sprintf("init containers has been injected into deployment %v", initContainer))

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					NodeSelector:   m.Spec.NodeSelector,
					InitContainers: initContainer,
					Containers: []corev1.Container{
						{
							Image:           m.Spec.Image,
							Name:            CSNAME,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Args:            arguments,
							SecurityContext: &priviledged,
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: "/home/coder/.local/share/code-server",
									Name:      "code-server-share-dir",
								},
								{
									MountPath: baseCodeDir,
									Name:      baseCodeVolume,
								},
							},
							Ports: []corev1.ContainerPort{{
								ContainerPort: HttpPort,
								Name:          "serverhttpport",
							}},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: baseCodeVolume,
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &dataVolume,
							},
						},
					},
				},
			},
		},
	}

	if secret != nil {
		reqLogger.Info(fmt.Sprintf("Found tls secret %s, will enable https for code server", secret.Name))
		secretSource := corev1.SecretVolumeSource{
			SecretName: r.Options.HttpsSecretName,
		}
		secretVolume := corev1.Volume{
			Name: "code-server-secret-vol",
			VolumeSource: corev1.VolumeSource{
				Secret: &secretSource,
			},
		}
		dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, secretVolume)
		for index, con := range dep.Spec.Template.Spec.Containers {
			if con.Name == CSNAME {
				secretsArgument := []string{"--cert", fmt.Sprintf("/etc/config/csserver/%s", CertFile), "--cert-key", fmt.Sprintf("/etc/config/csserver/%s", CertKey)}
				dep.Spec.Template.Spec.Containers[index].Args = append(dep.Spec.Template.Spec.Containers[index].Args, secretsArgument...)
				newVolumeMounts := []corev1.VolumeMount{
					{
						MountPath: "/etc/config/csserver",
						Name:      "code-server-secret-vol",
					},
				}
				dep.Spec.Template.Spec.Containers[index].VolumeMounts = append(
					dep.Spec.Template.Spec.Containers[index].VolumeMounts, newVolumeMounts...)
			}
		}
	}
	// Set CodeServer instance as the owner of the Deployment.
	controllerutil.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// deploymentForCodeServer returns a code server Deployment object
func (r *CodeServerReconciler) deploymentForGotty(m *csv1alpha1.CodeServer, secret *corev1.Secret) *appsv1.Deployment {
	reqLogger := r.Log.WithValues("namespace", m.Namespace, "name", m.Name)
	ls := labelsForCodeServer(m.Name)
	baseCodeDir := m.Spec.WorkspaceLocation
	baseCodeVolume := "code-server-workspace"
	replicas := int32(1)
	enablePriviledge := m.Spec.Privileged
	priviledged := corev1.SecurityContext{
		Privileged: enablePriviledge,
	}
	initContainer := r.addInitContainersForDeployment(m, baseCodeDir, baseCodeVolume)
	reqLogger.Info(fmt.Sprintf("init containers has been injected into deployment %v", initContainer))

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					InitContainers: initContainer,
					NodeSelector:   m.Spec.NodeSelector,
					Containers: []corev1.Container{
						{
							Image:           m.Spec.Image,
							Name:            CSNAME,
							Env:             m.Spec.Envs,
							Args:            m.Spec.Args,
							ImagePullPolicy: corev1.PullIfNotPresent,
							SecurityContext: &priviledged,
							VolumeMounts: []corev1.VolumeMount{
								{
									MountPath: baseCodeDir,
									Name:      baseCodeVolume,
								},
							},
							Resources: m.Spec.Resources,
						},
					},
				},
			},
		},
	}
	// add volume pvc pr emptyDir
	if r.needDeployPVC(m.Spec.StorageName) {
		dataVolume := corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: m.Name,
		}
		dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: baseCodeVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &dataVolume,
			},
		})
	} else {
		volumeQuantity, _ := resourcev1.ParseQuantity(m.Spec.StorageSize)
		dataVolume := corev1.EmptyDirVolumeSource{
			SizeLimit: &volumeQuantity,
		}
		dep.Spec.Template.Spec.Volumes = append(dep.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: baseCodeVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &dataVolume,
			},
		})
	}
	//https will be disabled no matter secret is provided or not. we only expose different port here.
	if secret != nil {
		for index, con := range dep.Spec.Template.Spec.Containers {
			if con.Name == CSNAME {
				dep.Spec.Template.Spec.Containers[index].Env = append(
					dep.Spec.Template.Spec.Containers[index].Env, corev1.EnvVar{
						Name:  "GOTTY_PORT",
						Value: strconv.Itoa(HttpsPort),
					})
				dep.Spec.Template.Spec.Containers[index].Ports = append(
					dep.Spec.Template.Spec.Containers[index].Ports, corev1.ContainerPort{
						ContainerPort: HttpsPort,
						Name:          "https",
					})
			}
		}
	} else {
		for index, con := range dep.Spec.Template.Spec.Containers {
			if con.Name == CSNAME {
				dep.Spec.Template.Spec.Containers[index].Env = append(
					dep.Spec.Template.Spec.Containers[index].Env, corev1.EnvVar{
						Name:  "GOTTY_PORT",
						Value: strconv.Itoa(HttpPort),
					})
				dep.Spec.Template.Spec.Containers[index].Ports = append(
					dep.Spec.Template.Spec.Containers[index].Ports, corev1.ContainerPort{
						ContainerPort: HttpPort,
						Name:          "http",
					})
			}
		}
	}
	// Append ingress and egress limit
	dep.Spec.Template.Annotations = map[string]string{}
	if len(m.Spec.IngressBandwidth) != 0 {
		dep.Spec.Template.Annotations[IngressLimitKey] = m.Spec.IngressBandwidth
	}
	if len(m.Spec.EgressBandwidth) != 0 {
		dep.Spec.Template.Annotations[EgressLimitKey] = m.Spec.EgressBandwidth
	}
	// Set CodeServer instance as the owner of the Deployment.
	controllerutil.SetControllerReference(m, dep, r.Scheme)
	return dep
}

// serviceForCodeServer function takes in a CodeServer object and returns a Service for that object.
func (r *CodeServerReconciler) serviceForCodeServer(m *csv1alpha1.CodeServer, secret *corev1.Secret) *corev1.Service {
	ls := labelsForCodeServer(m.Name)
	ser := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.Name,
			Namespace: m.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: ls,
		},
	}
	if secret == nil {
		ser.Spec.Ports = append(ser.Spec.Ports, corev1.ServicePort{
			Port:       HttpPort,
			Name:       "http",
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(HttpPort),
		})
	} else {
		ser.Spec.Ports = append(ser.Spec.Ports, corev1.ServicePort{
			Port:       HttpsPort,
			Name:       "https",
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(HttpsPort),
		})
	}
	// Set CodeServer instance as the owner of the Service.
	controllerutil.SetControllerReference(m, ser, r.Scheme)
	return ser
}

func (r *CodeServerReconciler) needDeployPVC(storageName string) bool {
	if storageName == StorageEmptyDir {
		return false
	}
	return true
}

// pvcForCodeServer function takes in a CodeServer object and returns a PersistentVolumeClaim for that object.
func (r *CodeServerReconciler) pvcForCodeServer(m *csv1alpha1.CodeServer) (*corev1.PersistentVolumeClaim, error) {
	pvcQuantity, err := resourcev1.ParseQuantity(m.Spec.StorageSize)
	if err != nil {
		return nil, err
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        m.Name,
			Namespace:   m.Namespace,
			Annotations: m.Spec.StorageAnnotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &m.Spec.StorageName,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: pvcQuantity,
				},
			},
		},
	}
	// Set CodeServer instance as the owner of the pvc.
	controllerutil.SetControllerReference(m, pvc, r.Scheme)
	return pvc, nil
}

func (r *CodeServerReconciler) getInstanceUrl(m *csv1alpha1.CodeServer) string {
	if len(r.Options.UrlPrefix) == 0 {
		//use the default prefix here. we need url prefix no matter it's configured or not
		return fmt.Sprintf("/%s", DefaultPrefix)
	} else {
		return fmt.Sprintf("/%s", strings.Trim(r.Options.UrlPrefix, "/"))
	}
}

// ingressForCodeServer function takes in a CodeServer object and returns a ingress for that object.
func (r *CodeServerReconciler) ingressForCodeServer(m *csv1alpha1.CodeServer, secret *corev1.Secret) *extv1.Ingress {
	servicePort := intstr.FromInt(HttpPort)
	if secret != nil {
		servicePort = intstr.FromInt(HttpsPort)
	}
	httpValue := extv1.HTTPIngressRuleValue{
		Paths: []extv1.HTTPIngressPath{
			{
				Path: fmt.Sprintf("%s(/|$)(.*)", r.getInstanceUrl(m)),
				Backend: extv1.IngressBackend{
					ServiceName: m.Name,
					ServicePort: servicePort,
				},
			},
		},
	}
	ingress := &extv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        m.Name,
			Namespace:   m.Namespace,
			Annotations: r.annotationsForIngress(m, secret),
		},
		Spec: extv1.IngressSpec{
			Rules: []extv1.IngressRule{
				{
					Host: fmt.Sprintf("%s.%s", m.Spec.Subdomain, r.Options.DomainName),
					IngressRuleValue: extv1.IngressRuleValue{
						HTTP: &httpValue,
					},
				},
			},
		},
	}
	if secret != nil {
		ingress.Spec.TLS = []extv1.IngressTLS{
			{
				Hosts:      []string{r.Options.DomainName},
				SecretName: r.Options.HttpsSecretName,
			},
		}
	}
	// Set CodeServer instance as the owner of the ingress.
	controllerutil.SetControllerReference(m, ingress, r.Scheme)
	return ingress
}

func (r *CodeServerReconciler) annotationsForIngress(m *csv1alpha1.CodeServer, secret *corev1.Secret) map[string]string {
	snippet := fmt.Sprintf(`proxy_set_header Accept-Encoding '';
sub_filter '<head>' '<head> <base href="%s/">';`, r.getInstanceUrl(m))
	annotation := map[string]string{
		"kubernetes.io/ingress.class":                       "nginx",
		"nginx.ingress.kubernetes.io/use-regex":             "true",
		"nginx.ingress.kubernetes.io/rewrite-target":        "/$2",
		"nginx.ingress.kubernetes.io/configuration-snippet": snippet,
	}

	// for the case of gotty we still use http for upstream connection
	if secret != nil && m.Spec.Runtime != csv1alpha1.RuntimeGotty {
		annotation["nginx.ingress.kubernetes.io/secure-backends"] = "true"
		annotation["nginx.ingress.kubernetes.io/backend-protocol"] = "HTTPS"
	}

	return annotation
}

// labelsForCodeServer returns the labels for selecting the resources
// belonging to the given CodeServer name.
func labelsForCodeServer(name string) map[string]string {
	return map[string]string{"app": "codeserver", "cs_name": name}
}

// NewStateCondition creates a new code server condition.
func NewStateCondition(conditionType csv1alpha1.ServerConditionType, reason string, message map[string]string) csv1alpha1.ServerCondition {
	return csv1alpha1.ServerCondition{
		Type:               conditionType,
		Status:             corev1.ConditionTrue,
		LastUpdateTime:     metav1.Now(),
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

// HasCondition checks whether the job has the specified condition
func HasCondition(status csv1alpha1.CodeServerStatus, condType csv1alpha1.ServerConditionType) bool {
	for _, condition := range status.Conditions {
		if condition.Type == condType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func HasDeploymentCondition(status appsv1.DeploymentStatus, condType appsv1.DeploymentConditionType) bool {
	for _, condition := range status.Conditions {
		if condition.Type == condType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// GetCondition gets the condition with specified condition type
func GetCondition(status csv1alpha1.CodeServerStatus, condType csv1alpha1.ServerConditionType) *csv1alpha1.ServerCondition {
	for _, condition := range status.Conditions {
		if condition.Type == condType {
			return &condition
		}
	}
	return nil
}

// SetCondition updates the code server status with provided condition
func SetCondition(status *csv1alpha1.CodeServerStatus, condition csv1alpha1.ServerCondition) {

	currentCond := GetCondition(*status, condition.Type)

	// Do nothing if condition doesn't change
	if currentCond != nil && currentCond.Status == condition.Status && currentCond.Reason == condition.Reason {
		return
	}

	// Do not update lastTransitionTime if the status of the condition doesn't change.
	if currentCond != nil && currentCond.Status == condition.Status {
		condition.LastTransitionTime = currentCond.LastTransitionTime
	}

	// Append the updated condition to the job status
	newConditions := filterOutCondition(status, condition)
	status.Conditions = append(newConditions, condition)
}

func filterOutCondition(states *csv1alpha1.CodeServerStatus, currentCondition csv1alpha1.ServerCondition) []csv1alpha1.ServerCondition {

	var newConditions []csv1alpha1.ServerCondition
	for _, condition := range states.Conditions {
		//Filter out the same condition
		if condition.Type == currentCondition.Type {
			continue
		}

		if currentCondition.Type == csv1alpha1.ServerCreated {
			break
		}

		if currentCondition.Type == csv1alpha1.ServerInactive || currentCondition.Type == csv1alpha1.ServerRecycled ||
			currentCondition.Type == csv1alpha1.ServerErrored {
			if currentCondition.Status == corev1.ConditionTrue && condition.Type == csv1alpha1.ServerReady {
				condition.Status = corev1.ConditionFalse
				condition.LastUpdateTime = metav1.Now()
				condition.LastTransitionTime = metav1.Now()
			}
		}

		if currentCondition.Type == csv1alpha1.ServerReady && currentCondition.Status == corev1.ConditionTrue {
			if condition.Type == csv1alpha1.ServerRecycled || condition.Type == csv1alpha1.ServerInactive ||
				currentCondition.Type == csv1alpha1.ServerErrored {
				condition.Status = corev1.ConditionFalse
				condition.LastUpdateTime = metav1.Now()
				condition.LastTransitionTime = metav1.Now()
			}
		}
		newConditions = append(newConditions, condition)
	}
	return newConditions
}

func needUpdatePVC(old, new *corev1.PersistentVolumeClaim) bool {
	return *old.Spec.StorageClassName != *new.Spec.StorageClassName ||
		!equality.Semantic.DeepEqual(old.Spec.Resources, new.Spec.Resources)
}

func needUpdateService(old, new *corev1.Service) bool {
	return !equality.Semantic.DeepEqual(old.Spec.Ports, new.Spec.Ports) ||
		!equality.Semantic.DeepEqual(old.Spec.Selector, new.Spec.Selector)
}

func needUpdateDeployment(old, new *appsv1.Deployment) bool {
	return !equality.Semantic.DeepEqual(old.Spec.Template.Spec.Volumes, new.Spec.Template.Spec.Volumes) ||
		!equality.Semantic.DeepEqual(old.Spec.Template.Spec.Containers, new.Spec.Template.Spec.Containers)
}

func (r *CodeServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	//watch codeserver, server, ingress, pvc and deployment.
	return ctrl.NewControllerManagedBy(mgr).
		For(&csv1alpha1.CodeServer{}).Owns(&corev1.Service{}).
		Owns(&extv1.Ingress{}).Owns(&appsv1.Deployment{}).Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
