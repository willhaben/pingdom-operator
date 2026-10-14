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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pingdomwillhabenatv1alpha1 "github.com/willhaben/pingdom-operator/api/v1alpha1"
)

type checkSpec = pingdomwillhabenatv1alpha1.CheckSpec

func secret(name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}, Data: map[string][]byte{}}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return s
}

func newTestReconciler(t *testing.T, objs ...client.Object) *CheckReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := pingdomwillhabenatv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &CheckReconciler{Client: c, Scheme: scheme, APIReader: c}
}

func TestMergeHeaders(t *testing.T) {
	tests := []struct {
		name    string
		headers []string
		secret  map[string][]byte
		want    []string
	}{
		{"no secret keeps plain headers", []string{"A:1"}, nil, []string{"A:1"}},
		{"secret keys are sorted after plain headers", []string{"X:1"}, map[string][]byte{"B": []byte("2"), "A": []byte("1")}, []string{"X:1", "A:1", "B:2"}},
		{"secret replaces a plain header ignoring case", []string{"user-agent:old", "X:1"}, map[string][]byte{"User-Agent": []byte("tok")}, []string{"X:1", "User-Agent:tok"}},
		{"secret value may contain a colon", nil, map[string][]byte{"Authorization": []byte("Basic a:b")}, []string{"Authorization:Basic a:b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeHeaders(tt.headers, tt.secret); !slices.Equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderURL(t *testing.T) {
	tests := []struct {
		name    string
		tmpl    string
		vars    map[string]string
		want    string
		wantErr string
	}{
		{"substitutes a secret value", "/a?k={{ .key }}", map[string]string{"key": "v"}, "/a?k=v", ""},
		{"urlquery escapes the value", "/a?k={{ .key | urlquery }}", map[string]string{"key": "a b&c"}, "/a?k=a+b%26c", ""},
		{"missing variable is an error naming it", "/{{ .nope }}", map[string]string{}, "", "nope"},
		{"malformed template is a parse error", "/{{ .key", nil, "", "parse urlTmpl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderURL(tt.tmpl, tt.vars)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestBuildRequestSecrets(t *testing.T) {
	const ns = "ns"
	r := newTestReconciler(t,
		secret("auth", map[string]string{"username": "u", "password": "p:w"}),
		secret("auth-nopass", map[string]string{"username": "u"}),
		secret("hdr", map[string]string{"X-Token": "t"}),
		secret("url", map[string]string{"key": "a b"}),
	)
	ref := func(n string) *corev1.LocalObjectReference { return &corev1.LocalObjectReference{Name: n} }

	tests := []struct {
		name     string
		spec     checkSpec
		check    func(*testing.T, string)
		wantErr  string
		wantSpec bool // error must be a *specError, so it is not retried
	}{
		{name: "auth is user:password", spec: checkSpec{AuthSecretRef: ref("auth")}, check: func(t *testing.T, got string) {
			if got != "u:p:w" {
				t.Errorf("Auth = %q", got)
			}
		}},
		{name: "auth secret missing a key", spec: checkSpec{AuthSecretRef: ref("auth-nopass")}, wantErr: "needs keys username and password"},
		{name: "auth secret not found", spec: checkSpec{AuthSecretRef: ref("nope")}, wantErr: `get auth Secret "nope"`},
		{name: "headers secret not found", spec: checkSpec{RequestHeadersSecretRef: ref("nope")}, wantErr: `get request headers Secret "nope"`},
		{name: "urlTmpl secret not found", spec: checkSpec{URLTmpl: "/{{ .key }}", URLTmplSecretRef: ref("nope")}, wantErr: `get url template Secret "nope"`},
		{name: "urlTmpl variable not in secret is a spec error", spec: checkSpec{URLTmpl: "/{{ .nope }}", URLTmplSecretRef: ref("url")}, wantErr: "nope", wantSpec: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &pingdomwillhabenatv1alpha1.Check{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c"}, Spec: tt.spec}
			req, err := r.buildRequest(context.Background(), c)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				if _, ok := errors.AsType[*specError](err); ok != tt.wantSpec {
					t.Errorf("specError = %v, want %v", ok, tt.wantSpec)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, req.Auth)
		})
	}

	t.Run("header and url secrets are applied", func(t *testing.T) {
		c := &pingdomwillhabenatv1alpha1.Check{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "c"},
			Spec:       checkSpec{RequestHeadersSecretRef: ref("hdr"), URLTmpl: "/s?k={{ .key | urlquery }}", URLTmplSecretRef: ref("url")},
		}
		req, err := r.buildRequest(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(req.RequestHeaders, []string{"X-Token:t"}) || req.URL != "/s?k=a+b" {
			t.Errorf("headers = %q, url = %q", req.RequestHeaders, req.URL)
		}
	})
}

func TestChecksForSecret(t *testing.T) {
	ref := func(n string) *corev1.LocalObjectReference { return &corev1.LocalObjectReference{Name: n} }
	check := func(ns, name string, spec checkSpec) client.Object {
		return &pingdomwillhabenatv1alpha1.Check{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	}
	r := newTestReconciler(t,
		check("ns", "uses-auth", checkSpec{AuthSecretRef: ref("s")}),
		check("ns", "uses-headers", checkSpec{RequestHeadersSecretRef: ref("s")}),
		check("ns", "uses-url", checkSpec{URLTmplSecretRef: ref("s")}),
		check("ns", "uses-other", checkSpec{AuthSecretRef: ref("other")}),
		check("ns", "uses-none", checkSpec{}),
		check("elsewhere", "uses-auth", checkSpec{AuthSecretRef: ref("s")}),
	)

	reqs := r.checksForSecret(context.Background(), secret("s", nil))
	got := make([]string, 0, len(reqs))
	for _, req := range reqs {
		got = append(got, req.String())
	}
	slices.Sort(got)
	want := []string{"ns/uses-auth", "ns/uses-headers", "ns/uses-url"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	if reqs := r.checksForSecret(context.Background(), secret("unrelated", nil)); len(reqs) != 0 {
		t.Errorf("unrelated secret mapped to %v", reqs)
	}
}
