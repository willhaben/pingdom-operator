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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pingdomwillhabenatv1alpha1 "github.com/willhaben/pingdom-operator/api/v1alpha1"
	"github.com/willhaben/pingdom-operator/internal/pingdom"
)

const (
	finalizer = "pingdom.willhaben.at/finalizer"

	// annotationCheckID adopts an existing Pingdom check: it is updated in place instead of creating a new one.
	annotationCheckID = "pingdom.willhaben.at/check-id"

	// resyncInterval is how often a check is sent to Pingdom again, which reverts edits made in the Pingdom UI.
	resyncInterval = 30 * time.Minute

	conditionReady = "Ready"

	reasonReconciling        = "Reconciling"
	reasonSynced             = "Synced"
	reasonInvalidSpec        = "InvalidSpec"
	reasonSecret             = "SecretUnavailable"
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

// +kubebuilder:rbac:groups=pingdom.willhaben.at,resources=checks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=pingdom.willhaben.at,resources=checks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pingdom.willhaben.at,resources=checks/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile syncs a Check to Pingdom: it creates the check once (or adopts the one named by the
// check-id annotation), updates it when the spec or a referenced Secret changes or every resyncInterval,
// and deletes it when the resource is deleted.
//
// shortcut: UI edits are reverted by the periodic update, not detected, add a GET and compare if drift must be reported.
// shortcut: a check deleted in the UI is reported as NotFound, not recreated.
// shortcut: a crash between POST and the status write leaks a Pingdom check, tag checks with the CR UID to adopt them.
// shortcut: bool and number fields (port, resolution, ...) and url removed from the spec keep their last value in Pingdom.
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

	body, err := r.buildRequest(ctx, check)
	if err != nil {
		reason := reasonSecret
		if _, ok := errors.AsType[*specError](err); ok {
			reason = reasonInvalidSpec
		}
		return r.fail(ctx, check, reason, err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return ctrl.Result{}, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	id := check.Status.CheckID
	if id != nil && check.Status.ObservedGeneration == check.Generation && check.Status.AppliedHash == hash {
		var wait time.Duration
		if t := check.Status.LastSyncTime; t != nil {
			wait = resyncInterval - time.Since(t.Time)
		}
		if wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}

	if id == nil {
		if v, ok := check.Annotations[annotationCheckID]; ok {
			adopted, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return r.fail(ctx, check, reasonInvalidSpec, fmt.Errorf("annotation %s must be a Pingdom check ID: %w", annotationCheckID, err))
			}
			id = &adopted
		}
	}

	if id == nil {
		newID, err := r.Pingdom.CreateCheck(ctx, body)
		if err != nil {
			return r.fail(ctx, check, "", err)
		}
		log.Info("Created Pingdom check", "checkID", newID)
		check.Status.CheckID = &newID
	} else {
		if err := r.Pingdom.UpdateCheck(ctx, *id, body); err != nil {
			return r.fail(ctx, check, "", err)
		}
		log.Info("Updated Pingdom check", "checkID", *id)
		check.Status.CheckID = id
	}

	now := metav1.Now()
	check.Status.ObservedGeneration = check.Generation
	check.Status.AppliedHash = hash
	check.Status.LastSyncTime = &now
	r.setReady(check, metav1.ConditionTrue, reasonSynced, "Check synced to Pingdom")
	if err := r.Status().Update(ctx, check); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
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
	case reason == reasonInvalidSpec: // cannot succeed until the spec changes
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
		data, err := r.secretData(ctx, check.Namespace, ref.Name, "auth")
		if err != nil {
			return nil, err
		}
		user, ok1 := data["username"]
		pass, ok2 := data["password"]
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("auth Secret %q needs keys username and password", ref.Name)
		}
		req.Auth = string(user) + ":" + string(pass)
	}
	if ref := s.RequestHeadersSecretRef; ref != nil {
		data, err := r.secretData(ctx, check.Namespace, ref.Name, "request headers")
		if err != nil {
			return nil, err
		}
		req.RequestHeaders = mergeHeaders(s.RequestHeaders, data)
	}
	if s.URLTmpl != "" {
		vars := map[string]string{}
		if ref := s.URLTmplSecretRef; ref != nil {
			data, err := r.secretData(ctx, check.Namespace, ref.Name, "url template")
			if err != nil {
				return nil, err
			}
			for k, v := range data {
				vars[k] = string(v)
			}
		}
		url, err := renderURL(s.URLTmpl, vars)
		if err != nil {
			return nil, &specError{err}
		}
		req.URL = url
	}
	return req, nil
}

// specError marks an error caused by the spec itself, which is not retried.
type specError struct{ error }

func (r *CheckReconciler) secretData(ctx context.Context, namespace, name, what string) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("get %s Secret %q: %w", what, name, err)
	}
	return secret.Data, nil
}

// mergeHeaders returns the "Name:value" headers plus one per Secret key, sorted by name.
// A Secret key replaces a header of the same name, ignoring case.
func mergeHeaders(headers []string, secret map[string][]byte) []string {
	keys := slices.Sorted(maps.Keys(secret))
	out := make([]string, 0, len(headers)+len(keys))
	for _, h := range headers {
		name, _, _ := strings.Cut(h, ":")
		if !slices.ContainsFunc(keys, func(k string) bool { return strings.EqualFold(k, name) }) {
			out = append(out, h)
		}
	}
	for _, k := range keys {
		out = append(out, k+":"+string(secret[k]))
	}
	return out
}

func renderURL(tmpl string, vars map[string]string) (string, error) {
	t, err := template.New("url").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse urlTmpl: %w", err)
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("render urlTmpl: %w", err)
	}
	return b.String(), nil
}

func joinInts(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ",")
}

// checksForSecret maps a Secret to the Checks that reference it, so a rotated token reaches Pingdom.
func (r *CheckReconciler) checksForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	checks := &pingdomwillhabenatv1alpha1.CheckList{}
	if err := r.List(ctx, checks, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Checks for Secret", "secret", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range checks.Items {
		c := &checks.Items[i]
		for _, ref := range []*corev1.LocalObjectReference{c.Spec.AuthSecretRef, c.Spec.RequestHeadersSecretRef, c.Spec.URLTmplSecretRef} {
			if ref != nil && ref.Name == obj.GetName() {
				reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(c)})
				break
			}
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *CheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pingdomwillhabenatv1alpha1.Check{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.checksForSecret)).
		Named("check").
		Complete(r)
}
