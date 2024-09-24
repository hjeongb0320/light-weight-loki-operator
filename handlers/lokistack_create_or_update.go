package handlers

import (
	"context"
	"fmt"
	"os"

	"github.com/ViaQ/logerr/v2/kverrors"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	lokiv1 "github.com/LokiGraduationProject/light-weight-loki-operator/api/v1"
	"github.com/LokiGraduationProject/light-weight-loki-operator/handlers/external/k8s"
	"github.com/LokiGraduationProject/light-weight-loki-operator/handlers/manifests"
	"github.com/LokiGraduationProject/light-weight-loki-operator/handlers/manifests/serviceaccounts"
	"github.com/LokiGraduationProject/light-weight-loki-operator/handlers/status"
	"github.com/LokiGraduationProject/light-weight-loki-operator/handlers/storage"
)

// CreateOrUpdateLokiStack handles LokiStack create and update events.
func CreateOrUpdateLokiStack(
	ctx context.Context,
	log logr.Logger,
	req ctrl.Request,
	k k8s.Client,
	s *runtime.Scheme,
) (lokiv1.CredentialMode, error) {
	ll := log.WithValues("lokistack", req.NamespacedName, "event", "createOrUpdate")

	var stack lokiv1.LokiStack
	if err := k.Get(ctx, req.NamespacedName, &stack); err != nil {
		if apierrors.IsNotFound(err) {
			// maybe the user deleted it before we could react? Either way this isn't an issue
			ll.Error(err, "could not find the requested loki stack", "name", req.NamespacedName)
			return "", nil
		}
		return "", kverrors.Wrap(err, "failed to lookup lokistack", "name", req.NamespacedName)
	}

	img := os.Getenv(manifests.EnvRelatedImageLoki)
	if img == "" {
		img = manifests.DefaultContainerImage
	}

	objStore, err := storage.BuildOptions(ctx, k, &stack)
	if err != nil {
		return "", err
	}

	// Here we will translate the lokiv1.LokiStack options into manifest options
	opts := manifests.Options{
		Name:          req.Name,
		Namespace:     req.Namespace,
		Image:         img,
		Stack:         stack.Spec,
		ObjectStorage: objStore,
	}

	ll.Info("begin building manifests")

	if optErr := manifests.ApplyDefaultSettings(&opts); optErr != nil {
		ll.Error(optErr, "failed to conform options to build settings")
		return "", optErr
	}

	objects, err := manifests.BuildAll(opts, log)
	if err != nil {
		ll.Error(err, "failed to build manifests")
		return "", err
	}

	// The status is updated before the objects are actually created to
	// avoid the scenario in which the configmap is successfully created or
	// updated and another resource is not. This would cause the status to
	// be possibly misaligned with the configmap, which could lead to
	// a user possibly being unable to read logs.
	if err := status.SetStorageSchemaStatus(ctx, k, req, objStore.Schemas); err != nil {
		ll.Error(err, "failed to set storage schema status")
		return "", err
	}

	var errCount int32

	for _, obj := range objects {
		l := ll.WithValues(
			"object_name", obj.GetName(),
			"object_kind", obj.GetObjectKind(),
		)

		if isNamespacedResource(obj) {
			obj.SetNamespace(req.Namespace)

			if err := ctrl.SetControllerReference(&stack, obj, s); err != nil {
				l.Error(err, "failed to set controller owner reference to resource")
				errCount++
				continue
			}
		}

		depAnnotations, err := dependentAnnotations(ctx, k, obj)
		if err != nil {
			l.Error(err, "failed to set dependent annotations")
			return "", err
		}

		desired := obj.DeepCopyObject().(client.Object)
		mutateFn := manifests.MutateFuncFor(obj, desired, depAnnotations)

		op, err := ctrl.CreateOrUpdate(ctx, k, obj, mutateFn)
		if err != nil {
			l.Error(err, "failed to configure resource")
			errCount++
			continue
		}

		msg := fmt.Sprintf("Resource has been %s", op)
		switch op {
		case ctrlutil.OperationResultNone:
			l.V(1).Info(msg)
		default:
			l.Info(msg)
		}
	}

	if errCount > 0 {
		return "", kverrors.New("failed to configure lokistack resources", "name", req.NamespacedName)
	}

	return objStore.CredentialMode, nil
}

// isNamespacedResource determines if an object should be managed or not by a LokiStack
func isNamespacedResource(obj client.Object) bool {
	switch obj.(type) {
	case *rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding:
		return false
	default:
		return true
	}
}

func dependentAnnotations(ctx context.Context, k k8s.Client, obj client.Object) (map[string]string, error) {
	a := obj.GetAnnotations()
	saName, ok := a[corev1.ServiceAccountNameKey]
	if !ok || saName == "" {
		return nil, nil
	}

	key := client.ObjectKey{Name: saName, Namespace: obj.GetNamespace()}
	uid, err := serviceaccounts.GetUID(ctx, k, key)
	if err != nil {
		return nil, err
	}

	return map[string]string{
		corev1.ServiceAccountUIDKey: uid,
	}, nil
}
