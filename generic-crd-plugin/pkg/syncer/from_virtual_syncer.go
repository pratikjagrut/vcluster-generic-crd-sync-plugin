package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/config"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/namecache"
	"github.com/loft-sh/vcluster-generic-crd-plugin/pkg/patches"
	"github.com/loft-sh/vcluster-sdk/syncer"
	synccontext "github.com/loft-sh/vcluster-sdk/syncer/context"
	"github.com/loft-sh/vcluster-sdk/syncer/translator"
	"github.com/loft-sh/vcluster-sdk/translate"
	"github.com/pkg/errors"
	"github.com/wI2L/jsondiff"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func CreateFromVirtualSyncer(ctx *synccontext.RegisterContext, config *config.FromVirtualCluster, nc namecache.NameCache) (syncer.Syncer, error) {
	obj := &unstructured.Unstructured{}
	obj.SetKind(config.Kind)
	obj.SetAPIVersion(config.ApiVersion)

	var err error
	var selector labels.Selector
	if config.Selector != nil && len(config.Selector.LabelSelector) > 0 {
		selector, err = metav1.LabelSelectorAsSelector(metav1.SetAsLabelSelector(config.Selector.LabelSelector))
		if err != nil {
			return nil, errors.Wrap(err, "parse label selector")
		}
	}

	statusIsSubresource := true
	// TODO: [low priority] check if config.Kind + config.ApiVersion has status subresource

	return &fromVirtualController{
		NamespacedTranslator: translator.NewNamespacedTranslator(ctx, config.Kind+"-from-virtual-syncer", obj),

		config:              config,
		namecache:           nc,
		selector:            selector,
		statusIsSubresource: statusIsSubresource,
	}, nil
}

type fromVirtualController struct {
	translator.NamespacedTranslator

	config              *config.FromVirtualCluster
	namecache           namecache.NameCache
	selector            labels.Selector
	statusIsSubresource bool
}

// func Resource() client.Object

func (f *fromVirtualController) SyncDown(ctx *synccontext.SyncContext, vObj client.Object) (ctrl.Result, error) {
	// check if selector matches
	if !f.objectMatches(vObj) {
		return ctrl.Result{}, nil
	}

	// new obj
	newObj := f.TranslateMetadata(vObj)

	err := patches.ApplyPatches(newObj, vObj, f.config.Patches, &virtualToHostNameResolver{namespace: vObj.GetNamespace()})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error applying patches: %v", err)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to apply declared patches to %s %s/%s: %v", f.config.Kind, newObj.GetNamespace(), newObj.GetName(), err)
	}

	ctx.Log.Infof("create physical %s %s/%s", f.config.Kind, newObj.GetNamespace(), newObj.GetName())
	err = ctx.PhysicalClient.Create(ctx.Context, newObj)
	if err != nil {
		ctx.Log.Infof("error syncing %s %s/%s to physical cluster: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
		f.EventRecorder().Eventf(vObj, "Warning", "SyncError", "Error syncing to physical cluster: %v", err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (f *fromVirtualController) Sync(ctx *synccontext.SyncContext, pObj client.Object, vObj client.Object) (ctrl.Result, error) {
	if !f.objectMatches(vObj) {
		ctx.Log.Infof("delete physical %s %s/%s, because it is not used anymore", f.config.Kind, pObj.GetNamespace(), pObj.GetName())
		err := ctx.PhysicalClient.Delete(ctx.Context, pObj)
		if err != nil {
			ctx.Log.Infof("error deleting physical %s %s/%s in physical cluster: %v", f.config.Kind, pObj.GetNamespace(), pObj.GetName(), err)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	//  |
	//  |
	//  |
	//  |
	// \|/
	// TODO: TOP priority - fix patching logic used below and in the back_syncer.Sync()
	// the changes done on the vObj should be done on pObj too,
	// but at the same time, rewriteName patches should use values of the vObj
	// as input, otherwise the value will be incorrectly rewritten in an infinite loop
	// Execute patches on physical object
	updatedPObj := pObj.DeepCopyObject().(client.Object)
	result, err := executeObjectPatch(ctx.Context, ctx.PhysicalClient, updatedPObj, func() error {
		err := patches.ApplyPatches(updatedPObj, vObj, f.config.Patches, &virtualToHostNameResolver{namespace: vObj.GetNamespace()})
		if err != nil {
			return fmt.Errorf("error applying patches: %v", err)
		}
		return nil
	})
	if err != nil {
		if kerrors.IsInvalid(err) {
			ctx.Log.Infof("Warning: this message could indicate a timing issue with no significant impact, or a bug. Please report this if your resource never reaches the expected state. Error message: failed to patch virtual %s %s/%s: %v", f.config.Kind, pObj.GetNamespace(), pObj.GetName(), err)
			// this happens when some field is being removed shortly after being added, which suggest it's a timing issue
			// it doesn't seem to have any negative consequence besides the logged error message
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to patch physical %s %s/%s: %v", f.config.Kind, pObj.GetNamespace(), pObj.GetName(), err)
	}
	if result == controllerutil.OperationResultUpdated || result == controllerutil.OperationResultUpdatedStatus || result == controllerutil.OperationResultUpdatedStatusOnly {
		// a change will trigger reconciliation anyway, and at that point we can make
		// a more accurate updates(reverse patches) to the virtual resource
		return ctrl.Result{}, nil
	}

	// Execute reverse patches on virtual object
	_, err = executeObjectPatch(ctx.Context, ctx.VirtualClient, vObj, func() error {
		err = patches.ApplyPatches(vObj, pObj, f.config.ReversePatches, &hostToVirtualNameResolver{namecache: f.namecache})
		if err != nil {
			return fmt.Errorf("error applying patches: %v", err)
		}
		return nil
	})
	if err != nil {
		if kerrors.IsInvalid(err) {
			ctx.Log.Infof("Warning: this message could indicate a timing issue with no significant impact, or a bug. Please report this if your resource never reaches the expected state. Error message: failed to patch virtual %s %s/%s: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
			// this happens when some field is being removed shortly after being added, which suggest it's a timing issue
			// it doesn't seem to have any negative consequence besides the logged error message
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to patch virtual %s %s/%s: %v", f.config.Kind, vObj.GetNamespace(), vObj.GetName(), err)
	}

	return ctrl.Result{}, nil
}

type MutateFn func() error

func executeObjectPatch(ctx context.Context, c client.Client, obj client.Object, f MutateFn) (controllerutil.OperationResult, error) {
	//TODO: we can simplify this function by a lot, aplly the reversePatches on the vObj, produce the json.Diff
	// and then split the resulting diff into to two - changes to the status + all else
	// Current implementation is based on controllerutil.CreateOrPatch

	var updated, statusUpdated bool
	statusIsSubresource := true // do we need to skip status subresource Patch on the resource that don't have status as subresource?

	// Create a copy of the original object as well as converting that copy to
	// unstructured data.
	before, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj.DeepCopyObject())
	if err != nil {
		return controllerutil.OperationResultNone, err
	}
	beforeWithStatus := make(map[string]interface{})
	for k, v := range before {
		beforeWithStatus[k] = v
	}

	// Attempt to extract the status from the resource for easier comparison later
	beforeStatus, hasBeforeStatus, err := unstructured.NestedFieldCopy(before, "status")
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// If the resource contains a status then remove it from the unstructured
	// copy to avoid unnecessary patching later.
	if hasBeforeStatus && statusIsSubresource {
		unstructured.RemoveNestedField(before, "status")
	}

	// Mutate the original object.
	err = f()
	if err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("failed to apply declared patches to %s %s/%s: %v", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	// Convert the resource to unstructured to compare against our before copy.
	after, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// Attempt to extract the status from the resource for easier comparison later
	afterStatus, hasAfterStatus, err := unstructured.NestedFieldCopy(after, "status")
	if err != nil {
		return controllerutil.OperationResultNone, err
	}

	// If the resource contains a status then remove it from the unstructured
	// copy to avoid unnecessary patching later.
	if hasAfterStatus && statusIsSubresource {
		unstructured.RemoveNestedField(after, "status")
	}

	if !reflect.DeepEqual(before, after) {
		// Only issue a Patch if the before and after resources (minus status) differ

		patch, err := jsondiff.Compare(before, after)
		if err != nil {
			return controllerutil.OperationResultNone, err
		}
		patchBytes, err := json.Marshal(patch)
		if err != nil {
			return controllerutil.OperationResultNone, err
		}

		err = c.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, patchBytes))
		if err != nil {
			return controllerutil.OperationResultNone, err
		}
		updated = true
	}

	if statusIsSubresource && (hasBeforeStatus || hasAfterStatus) && !reflect.DeepEqual(beforeStatus, afterStatus) {
		// Only issue a Status Patch if the resource has a status and the beforeStatus
		// and afterStatus copies differ
		objectAfterPatch, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}
		if err = unstructured.SetNestedField(objectAfterPatch, afterStatus, "status"); err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}
		// If Status was replaced by Patch before, restore patched structure to the obj
		if err = runtime.DefaultUnstructuredConverter.FromUnstructured(objectAfterPatch, obj); err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}

		statusPatch, err := jsondiff.Compare(beforeWithStatus, objectAfterPatch)
		if err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}
		statusPatchBytes, err := json.Marshal(statusPatch)
		if err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}

		if err := c.Status().Patch(ctx, obj, client.RawPatch(types.JSONPatchType, statusPatchBytes)); err != nil {
			if updated {
				return controllerutil.OperationResultUpdated, err
			} else {
				return controllerutil.OperationResultNone, err
			}
		}
		statusUpdated = true
	}
	if updated && statusUpdated {
		return controllerutil.OperationResultUpdatedStatus, nil
	} else if updated && !statusUpdated {
		return controllerutil.OperationResultUpdated, nil
	} else if !updated && statusUpdated {
		return controllerutil.OperationResultUpdatedStatusOnly, nil
	} else {
		return controllerutil.OperationResultNone, err
	}
}

func (f *fromVirtualController) objectMatches(obj client.Object) bool {
	return f.selector == nil || !f.selector.Matches(labels.Set(obj.GetLabels()))
}

type virtualToHostNameResolver struct {
	namespace string
}

func (r *virtualToHostNameResolver) TranslateName(name string, _ string) (string, error) {
	return translate.PhysicalName(name, r.namespace), nil
}

type hostToVirtualNameResolver struct {
	namecache namecache.NameCache
}

func (r *hostToVirtualNameResolver) TranslateName(name string, path string) (string, error) {
	n := r.namecache.ResolveName(name, path)
	if n.Name == "" {
		return "", fmt.Errorf("could not translate %s host resource name to vcluster resource name", name)
	}

	return n.Name, nil
}
