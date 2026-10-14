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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pingdomwillhabenatv1alpha1 "github.com/willhaben/pingdom-operator/api/v1alpha1"
	"github.com/willhaben/pingdom-operator/internal/pingdom"
)

type fakePingdom struct {
	created, updated []*pingdom.CheckRequest
	updatedIDs       []int64
	deletedIDs       []int64
	err              error
}

func (f *fakePingdom) CreateCheck(_ context.Context, r *pingdom.CheckRequest) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.created = append(f.created, r)
	return 42, nil
}

func (f *fakePingdom) UpdateCheck(_ context.Context, id int64, r *pingdom.CheckRequest) error {
	if f.err != nil {
		return f.err
	}
	f.updated = append(f.updated, r)
	f.updatedIDs = append(f.updatedIDs, id)
	return nil
}

func (f *fakePingdom) DeleteCheck(_ context.Context, id int64) error {
	f.deletedIDs = append(f.deletedIDs, id)
	return f.err
}

var _ = Describe("Check Controller", func() {
	const resourceName = "test-resource"

	ctx := context.Background()
	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	var fake *fakePingdom
	var reconciler *CheckReconciler
	reconcileOnce := func() (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	get := func() *pingdomwillhabenatv1alpha1.Check {
		c := &pingdomwillhabenatv1alpha1.Check{}
		Expect(k8sClient.Get(ctx, key, c)).To(Succeed())
		return c
	}

	BeforeEach(func() {
		fake = &fakePingdom{}
		reconciler = &CheckReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Pingdom: fake, APIReader: k8sClient}
		Expect(k8sClient.Create(ctx, &pingdomwillhabenatv1alpha1.Check{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: key.Namespace},
			Spec:       pingdomwillhabenatv1alpha1.CheckSpec{Host: "example.com", Type: "http"},
		})).To(Succeed())
	})

	AfterEach(func() {
		c := &pingdomwillhabenatv1alpha1.Check{}
		if err := k8sClient.Get(ctx, key, c); err == nil {
			c.Finalizers = nil
			Expect(k8sClient.Update(ctx, c)).To(Succeed())
			_ = k8sClient.Delete(ctx, c)
		}
	})

	It("creates the check once, updates on spec change, and deletes with the resource", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created).To(HaveLen(1))
		Expect(fake.created[0].Name).To(Equal(resourceName))
		c := get()
		Expect(*c.Status.CheckID).To(Equal(int64(42)))
		Expect(c.Finalizers).To(ContainElement(finalizer))
		Expect(meta.IsStatusConditionTrue(c.Status.Conditions, conditionReady)).To(BeTrue())

		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created).To(HaveLen(1))
		Expect(fake.updated).To(BeEmpty())

		c.Spec.Paused = true
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.updated).To(HaveLen(1))
		Expect(fake.updated[0].Paused).To(BeTrue())

		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.deletedIDs).To(Equal([]int64{42}))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &pingdomwillhabenatv1alpha1.Check{}))).To(BeTrue())
	})

	It("does not retry client errors and reports them on the Ready condition", func() {
		fake.err = &pingdom.Error{StatusCode: 400, Message: "Invalid host"}
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		cond := meta.FindStatusCondition(get().Status.Conditions, conditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(reasonInvalidSpec))
		Expect(cond.Message).To(ContainSubstring("Invalid host"))
	})

	It("returns server errors so they are retried", func() {
		fake.err = &pingdom.Error{StatusCode: 503, Message: "down"}
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonPingdomUnavailable))
	})

	It("requeues on rate limiting without returning an error", func() {
		fake.err = &pingdom.Error{StatusCode: 429, Message: "slow down", RetryAfter: 90 * time.Second}
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(90 * time.Second))
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonRateLimited))
	})

	It("reports a check deleted in Pingdom as NotFound", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		c := get()
		c.Spec.Paused = true
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		fake.err = &pingdom.Error{StatusCode: 403, Message: "Invalid check identifier"}
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonNotFound))
	})

	It("reports an auth failure as Unauthorized", func() {
		fake.err = &pingdom.Error{StatusCode: 401, Message: "bad token"}
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonUnauthorized))
	})

	It("retries when the auth Secret is missing", func() {
		c := get()
		c.Spec.AuthSecretRef = &corev1.LocalObjectReference{Name: "missing"}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).To(HaveOccurred())
		Expect(fake.created).To(BeEmpty())
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonSecret))
	})

	It("sends user:password from the auth Secret", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: key.Namespace},
			StringData: map[string]string{"username": "u", "password": "p:w"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, secret)).To(Succeed()) })

		c := get()
		c.Spec.AuthSecretRef = &corev1.LocalObjectReference{Name: "auth"}
		c.Spec.UserIDs = []int64{1, 2}
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created[0].Auth).To(Equal("u:p:w"))
		Expect(fake.created[0].UserIDs).To(Equal("1,2"))
	})

	It("treats an already deleted Pingdom check as deleted", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		fake.err = &pingdom.Error{StatusCode: 403, Message: "Invalid check identifier"} // what Pingdom really returns
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, &pingdomwillhabenatv1alpha1.Check{}))).To(BeTrue())
	})

	It("keeps the finalizer when Pingdom deletion fails", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		fake.err = &pingdom.Error{StatusCode: 500, Message: "boom"}
		Expect(k8sClient.Delete(ctx, get())).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).To(HaveOccurred())
		c := get()
		Expect(c.Finalizers).To(ContainElement(finalizer))
		Expect(meta.FindStatusCondition(c.Status.Conditions, conditionReady).Reason).To(Equal(reasonDeleteFailed))
	})

	newSecret := func(name string, data map[string]string) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace}, StringData: data}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, s)).To(Succeed()) })
		return s
	}
	update := func(f func(*pingdomwillhabenatv1alpha1.Check)) {
		c := get()
		f(c)
		Expect(k8sClient.Update(ctx, c)).To(Succeed())
	}
	ref := func(name string) *corev1.LocalObjectReference { return &corev1.LocalObjectReference{Name: name} }

	It("sends plain and Secret headers sorted, the Secret replacing a plain header of the same name", func() {
		newSecret("hdr", map[string]string{"User-Agent": "tok", "Authorization": "Bearer x"})
		update(func(c *pingdomwillhabenatv1alpha1.Check) {
			c.Spec.RequestHeaders = []string{"user-agent:old", "X-WH-Client:pingdom"}
			c.Spec.RequestHeadersSecretRef = ref("hdr")
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created[0].RequestHeaders).To(Equal([]string{"X-WH-Client:pingdom", "Authorization:Bearer x", "User-Agent:tok"}))
	})

	It("renders urlTmpl with the Secret values", func() {
		newSecret("url", map[string]string{"key": "a b&c"})
		update(func(c *pingdomwillhabenatv1alpha1.Check) {
			c.Spec.URLTmpl = "/search?key={{ .key | urlquery }}&q=test"
			c.Spec.URLTmplSecretRef = ref("url")
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created[0].URL).To(Equal("/search?key=a+b%26c&q=test"))
	})

	It("reports a urlTmpl variable that is not in the Secret as InvalidSpec without retrying", func() {
		update(func(c *pingdomwillhabenatv1alpha1.Check) { c.Spec.URLTmpl = "/{{ .nope }}" })
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created).To(BeEmpty())
		cond := meta.FindStatusCondition(get().Status.Conditions, conditionReady)
		Expect(cond.Reason).To(Equal(reasonInvalidSpec))
		Expect(cond.Message).To(ContainSubstring("nope"))
	})

	It("rejects url together with urlTmpl", func() {
		c := get()
		c.Spec.URL, c.Spec.URLTmpl = "/a", "/b"
		Expect(k8sClient.Update(ctx, c)).To(MatchError(ContainSubstring("url and urlTmpl are mutually exclusive")))
	})

	It("sends the check again when a referenced Secret changes", func() {
		s := newSecret("hdr", map[string]string{"X-Token": "1"})
		update(func(c *pingdomwillhabenatv1alpha1.Check) { c.Spec.RequestHeadersSecretRef = ref("hdr") })
		Expect(reconciler.checksForSecret(ctx, s)).To(ConsistOf(reconcile.Request{NamespacedName: key}))
		Expect(reconciler.checksForSecret(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: key.Namespace}})).To(BeEmpty())

		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.updated).To(BeEmpty())

		s.StringData = map[string]string{"X-Token": "2"}
		Expect(k8sClient.Update(ctx, s)).To(Succeed())
		_, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.updated).To(HaveLen(1))
		Expect(fake.updated[0].RequestHeaders).To(Equal([]string{"X-Token:2"}))
	})

	It("sends the check again after the resync interval", func() {
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		res, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.updated).To(BeEmpty())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", resyncInterval))

		c := get()
		old := metav1.NewTime(time.Now().Add(-resyncInterval - time.Minute))
		c.Status.LastSyncTime = &old
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		res, err = reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.updated).To(HaveLen(1))
		Expect(fake.updatedIDs).To(Equal([]int64{42}))
		Expect(res.RequeueAfter).To(Equal(resyncInterval))
	})

	It("adopts the Pingdom check named by the check-id annotation", func() {
		update(func(c *pingdomwillhabenatv1alpha1.Check) {
			c.Annotations = map[string]string{annotationCheckID: "77"}
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created).To(BeEmpty())
		Expect(fake.updatedIDs).To(Equal([]int64{77}))
		Expect(*get().Status.CheckID).To(Equal(int64(77)))
	})

	It("reports a malformed check-id annotation as InvalidSpec without retrying", func() {
		update(func(c *pingdomwillhabenatv1alpha1.Check) {
			c.Annotations = map[string]string{annotationCheckID: "abc"}
		})
		_, err := reconcileOnce()
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.created).To(BeEmpty())
		Expect(meta.FindStatusCondition(get().Status.Conditions, conditionReady).Reason).To(Equal(reasonInvalidSpec))
	})

	It("rejects an immutable type change", func() {
		c := get()
		c.Spec.Type = "ping"
		Expect(k8sClient.Update(ctx, c)).To(MatchError(ContainSubstring("type is immutable")))
	})

	It("rejects specs missing fields required by the type", func() {
		bad := &pingdomwillhabenatv1alpha1.Check{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-tcp", Namespace: key.Namespace},
			Spec:       pingdomwillhabenatv1alpha1.CheckSpec{Host: "example.com", Type: "tcp"},
		}
		Expect(k8sClient.Create(ctx, bad)).To(MatchError(ContainSubstring("port is required for tcp checks")))
	})
})
