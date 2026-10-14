/*
Copyright 2026.

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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pingdomwillhabenatv1alpha1 "github.com/willhaben/pingdom-operator/api/v1alpha1"
	"github.com/willhaben/pingdom-operator/internal/pingdom"
)

const (
	finalizer = "pingdom.willhaben.at/finalizer"

	conditionReady = "Ready"

	reasonReconciling        = "Reconciling"
	reasonSynced             = "Synced"
	reasonInvalidSpec        = "InvalidSpec"
	reasonAuthSecret         = "AuthSecretUnavailable"
	reasonUnauthorized       = "Unauthorized"
	reasonNotFound           = "NotFound"
	reasonRateLimited        = "RateLimited"
	reasonPingdomUnavailable = "PingdomUnavailable"
	reasonDeleteFailed       = "DeleteFailed"
)

// PingdomClient is the subset of the Pingdom API used by the reconciler.
type PingdomClient interface {
	CreateCheck(ctx context.Context, req *pingdom.CheckRequest) (int64, error)
	UpdateCheck(ctx context.Context, id int64, req *pingdom.CheckRequest) error
	DeleteCheck(ctx context.Context, id int64) error
}

// CheckReconciler reconciles a Check object
type CheckReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Pingdom PingdomClient
	// APIReader reads straight from the API server. The cached client can lag behind our own
	// status write, which would make a second reconcile create a duplicate Pingdom check.
	APIReader client.Reader
}

// +kubebuilder:rbac:groups=pingdom.willhaben.at.willhaben.at,resources=checks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pingdom.willhaben.at.willhaben.at,resources=checks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pingdom.willhaben.at.willhaben.at,resources=checks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile syncs a Check to Pingdom: it creates the check once, updates it when the spec
// generation changes and deletes it when the resource is deleted.
//
// shortcut: Pingdom-side drift (edits or deletes in the UI) is not detected, add a periodic GET if needed.
// shortcut: a crash between POST and the status write leaks a Pingdom check, tag checks with the CR UID to adopt them.
// shortcut: fields removed from the spec are not cleared in Pingdom, PUT leaves omitted fields unchanged.
func (r *CheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	check := &pingdomwillhabenatv1alpha1.Check{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, check); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !check.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, check)
	}

	if controllerutil.AddFinalizer(check, finalizer) {
		if err := r.Update(ctx, check); err != nil {
			return ctrl.Result{}, err
		}
	}

	if meta.FindStatusCondition(check.Status.Conditions, conditionReady) == nil {
		r.setReady(check, metav1.ConditionUnknown, reasonReconciling, "Starting reconciliation")
		if err := r.Status().Update(ctx, check); err != nil {
			return ctrl.Result{}, err
		}
	}

	id := check.Status.CheckID
	if id != nil && check.Status.ObservedGeneration == check.Generation {
		return ctrl.Result{}, nil
	}

	body, err := r.buildRequest(ctx, check)
	if err != nil {
		return r.fail(ctx, check, reasonAuthSecret, err)
	}

	if id == nil {
		newID, err := r.Pingdom.CreateCheck(ctx, body)
		if err != nil {
			return r.fail(ctx, check, "", err)
		}
		log.Info("Created Pingdom check", "checkID", newID)
		check.Status.CheckID = &newID
	} else {
		body.Type = "" // Pingdom does not allow changing the type
		if err := r.Pingdom.UpdateCheck(ctx, *id, body); err != nil {
			return r.fail(ctx, check, "", err)
		}
		log.Info("Updated Pingdom check", "checkID", *id)
	}

	check.Status.ObservedGeneration = check.Generation
	r.setReady(check, metav1.ConditionTrue, reasonSynced, "Check synced to Pingdom")
	return ctrl.Result{}, r.Status().Update(ctx, check)
}

func (r *CheckReconciler) finalize(ctx context.Context, check *pingdomwillhabenatv1alpha1.Check) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(check, finalizer) {
		return ctrl.Result{}, nil
	}
	if id := check.Status.CheckID; id != nil {
		var perr *pingdom.Error
		if err := r.Pingdom.DeleteCheck(ctx, *id); err != nil && (!errors.As(err, &perr) || !perr.NotFound()) {
			r.setReady(check, metav1.ConditionFalse, reasonDeleteFailed, err.Error())
			if uerr := r.Status().Update(ctx, check); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{}, err
		}
		logf.FromContext(ctx).Info("Deleted Pingdom check", "checkID", *id)
	}
	controllerutil.RemoveFinalizer(check, finalizer)
	return ctrl.Result{}, r.Update(ctx, check)
}

// fail records err on the Ready condition. Client errors other than 429 are not retried,
// since they cannot succeed until the spec or token changes.
func (r *CheckReconciler) fail(ctx context.Context, check *pingdomwillhabenatv1alpha1.Check, reason string, err error) (ctrl.Result, error) {
	result, retryErr := ctrl.Result{}, error(nil)
	var perr *pingdom.Error
	switch {
	case reason != "": // local error, e.g. missing Secret
		retryErr = err
	case !errors.As(err, &perr):
		reason, retryErr = reasonPingdomUnavailable, err
	case perr.StatusCode == 429:
		reason, result.RequeueAfter = reasonRateLimited, perr.RetryAfter
	case perr.NotFound():
		reason = reasonNotFound
	case perr.StatusCode == 401 || perr.StatusCode == 403:
		reason = reasonUnauthorized
	case perr.StatusCode >= 500:
		reason, retryErr = reasonPingdomUnavailable, err
	default:
		reason = reasonInvalidSpec
	}
	logf.FromContext(ctx).Error(err, "Failed to sync Pingdom check", "reason", reason)
	r.setReady(check, metav1.ConditionFalse, reason, err.Error())
	if uerr := r.Status().Update(ctx, check); uerr != nil {
		return ctrl.Result{}, uerr
	}
	return result, retryErr
}

func (r *CheckReconciler) setReady(check *pingdomwillhabenatv1alpha1.Check, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&check.Status.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: check.Generation,
	})
}

func (r *CheckReconciler) buildRequest(ctx context.Context, check *pingdomwillhabenatv1alpha1.Check) (*pingdom.CheckRequest, error) {
	s := &check.Spec
	req := &pingdom.CheckRequest{
		Name:                     s.Name,
		Host:                     s.Host,
		Type:                     string(s.Type),
		Paused:                   s.Paused,
		Resolution:               s.Resolution,
		UserIDs:                  joinInts(s.UserIDs),
		TeamIDs:                  joinInts(s.TeamIDs),
		IntegrationIDs:           s.IntegrationIDs,
		SendNotificationWhenDown: s.SendNotificationWhenDown,
		NotifyAgainEvery:         s.NotifyAgainEvery,
		NotifyWhenBackUp:         s.NotifyWhenBackUp,
		Tags:                     s.Tags,
		ProbeFilters:             s.ProbeFilters,
		IPv6:                     s.IPv6,
		ResponseTimeThreshold:    s.ResponseTimeThreshold,
		CustomMessage:            s.CustomMessage,
		Port:                     s.Port,
		Encryption:               s.Encryption,
		URL:                      s.URL,
		ShouldContain:            s.ShouldContain,
		ShouldNotContain:         s.ShouldNotContain,
		PostData:                 s.PostData,
		RequestHeaders:           s.RequestHeaders,
		VerifyCertificate:        s.VerifyCertificate,
		SSLDownDaysBefore:        s.SSLDownDaysBefore,
		AdditionalURLs:           s.AdditionalURLs,
		StringToSend:             s.StringToSend,
		StringToExpect:           s.StringToExpect,
		Nameserver:               s.Nameserver,
		ExpectedIP:               s.ExpectedIP,
	}
	if req.Name == "" {
		req.Name = check.Name
	}
	if ref := s.AuthSecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: check.Namespace, Name: ref.Name}, secret); err != nil {
			return nil, fmt.Errorf("get auth Secret %q: %w", ref.Name, err)
		}
		user, ok1 := secret.Data["username"]
		pass, ok2 := secret.Data["password"]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("auth Secret %q needs keys username and password", ref.Name)
		}
		req.Auth = string(user) + ":" + string(pass)
	}
	return req, nil
}

func joinInts(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ",")
}

// SetupWithManager sets up the controller with the Manager.
func (r *CheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pingdomwillhabenatv1alpha1.Check{}).
		Named("check").
		Complete(r)
}
