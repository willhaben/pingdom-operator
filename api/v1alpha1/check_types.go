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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// CheckType is the Pingdom check type.
// +kubebuilder:validation:Enum=http;httpcustom;tcp;ping;dns;udp;smtp;pop3;imap
type CheckType string

// CheckSpec defines the desired state of Check, mirroring the Pingdom API 3.1 POST /checks body.
// +kubebuilder:validation:XValidation:rule="self.type != 'tcp' || has(self.port)",message="port is required for tcp checks"
// +kubebuilder:validation:XValidation:rule="self.type != 'udp' || (has(self.port) && has(self.stringToSend) && has(self.stringToExpect))",message="port, stringToSend and stringToExpect are required for udp checks"
// +kubebuilder:validation:XValidation:rule="self.type != 'dns' || (has(self.nameserver) && has(self.expectedIP))",message="nameserver and expectedIP are required for dns checks"
// +kubebuilder:validation:XValidation:rule="self.type != 'httpcustom' || has(self.url)",message="url is required for httpcustom checks"
// +kubebuilder:validation:XValidation:rule="!(has(self.shouldContain) && has(self.shouldNotContain))",message="shouldContain and shouldNotContain are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!(has(self.url) && has(self.urlTmpl))",message="url and urlTmpl are mutually exclusive"
type CheckSpec struct {
	// name of the check in Pingdom. Defaults to the resource name.
	// +optional
	Name string `json:"name,omitempty"`

	// host is the target host.
	// +kubebuilder:validation:MinLength=1
	// +required
	Host string `json:"host"`

	// type of the check. It cannot be changed after creation.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="type is immutable"
	// +required
	Type CheckType `json:"type"`

	// paused pauses the check.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// resolution is how often the check is tested, in minutes.
	// +kubebuilder:validation:Enum=1;5;15;30;60
	// +optional
	Resolution *int32 `json:"resolution,omitempty"`

	// userIDs are the Pingdom user IDs to alert.
	// +optional
	UserIDs []int64 `json:"userIDs,omitempty"`

	// teamIDs are the Pingdom team IDs to alert.
	// +optional
	TeamIDs []int64 `json:"teamIDs,omitempty"`

	// integrationIDs are the Pingdom integration IDs to alert.
	// +optional
	IntegrationIDs []int64 `json:"integrationIDs,omitempty"`

	// sendNotificationWhenDown sends a notification after the check was down this many times.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SendNotificationWhenDown *int32 `json:"sendNotificationWhenDown,omitempty"`

	// notifyAgainEvery re-notifies every n results. 0 means no extra notifications.
	// +kubebuilder:validation:Minimum=0
	// +optional
	NotifyAgainEvery *int32 `json:"notifyAgainEvery,omitempty"`

	// notifyWhenBackUp sends a notification when the check is up again.
	// +optional
	NotifyWhenBackUp *bool `json:"notifyWhenBackUp,omitempty"`

	// tags of the check. Max 64 characters each.
	// +kubebuilder:validation:items:MaxLength=64
	// +optional
	Tags []string `json:"tags,omitempty"`

	// probeFilters selects probes, for example "region: NA". Only region (EU, NA, APAC, LATAM) is supported.
	// +optional
	ProbeFilters []string `json:"probeFilters,omitempty"`

	// ipv6 uses IPv6 instead of IPv4.
	// +optional
	IPv6 *bool `json:"ipv6,omitempty"`

	// responseTimeThreshold triggers a down alert above this response time in ms (not on Starter and Free plans).
	// +kubebuilder:validation:Minimum=1
	// +optional
	ResponseTimeThreshold *int32 `json:"responseTimeThreshold,omitempty"`

	// customMessage is added to email and webhook alerts.
	// +optional
	CustomMessage string `json:"customMessage,omitempty"`

	// port of the target. Used by http, httpcustom, tcp, udp, smtp, pop3 and imap checks.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`

	// encryption enables TLS. Used by http, httpcustom and smtp checks.
	// +optional
	Encryption *bool `json:"encryption,omitempty"`

	// url is the path on the server. Used by http and httpcustom checks.
	// +optional
	URL string `json:"url,omitempty"`

	// urlTmpl is a Go template for the url, for values that come from a Secret, for example
	// "/search?key={{ .key | urlquery }}". The variables are the keys of urlTmplSecretRef.
	// Mutually exclusive with url.
	// +optional
	URLTmpl string `json:"urlTmpl,omitempty"`

	// urlTmplSecretRef names a Secret in the same namespace whose keys are the urlTmpl variables.
	// +optional
	URLTmplSecretRef *corev1.LocalObjectReference `json:"urlTmplSecretRef,omitempty"`

	// shouldContain is a string the response must contain. Used by http checks.
	// +optional
	ShouldContain string `json:"shouldContain,omitempty"`

	// shouldNotContain is a string the response must not contain. Used by http checks.
	// +optional
	ShouldNotContain string `json:"shouldNotContain,omitempty"`

	// postData is sent as the request body. Used by http checks.
	// +optional
	PostData string `json:"postData,omitempty"`

	// requestHeaders are custom headers as "Name:value". Used by http checks.
	// +optional
	RequestHeaders []string `json:"requestHeaders,omitempty"`

	// requestHeadersSecretRef names a Secret in the same namespace whose keys are header names and
	// values are header values, for example a token. They are added to requestHeaders and win on a name clash.
	// +optional
	RequestHeadersSecretRef *corev1.LocalObjectReference `json:"requestHeadersSecretRef,omitempty"`

	// verifyCertificate treats the target as down on an invalid certificate. Used by http and httpcustom checks.
	// +optional
	VerifyCertificate *bool `json:"verifyCertificate,omitempty"`

	// sslDownDaysBefore treats the target as down if the certificate expires within this many days.
	// +kubebuilder:validation:Minimum=0
	// +optional
	SSLDownDaysBefore *int32 `json:"sslDownDaysBefore,omitempty"`

	// additionalURLs is the full URL of an additional XML file. Used by httpcustom checks.
	// +optional
	AdditionalURLs string `json:"additionalURLs,omitempty"`

	// stringToSend is sent to the target. Used by tcp and udp checks.
	// +optional
	StringToSend string `json:"stringToSend,omitempty"`

	// stringToExpect must appear in the response. Used by tcp, udp, smtp, pop3 and imap checks.
	// +optional
	StringToExpect string `json:"stringToExpect,omitempty"`

	// nameserver is the DNS server to query. Used by dns checks.
	// +optional
	Nameserver string `json:"nameserver,omitempty"`

	// expectedIP is the IP the DNS server must return. Used by dns checks.
	// +optional
	ExpectedIP string `json:"expectedIP,omitempty"`

	// authSecretRef names a Secret in the same namespace with keys "username" and "password",
	// used for target authentication by http, httpcustom and smtp checks.
	// +optional
	AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`
}

// CheckStatus defines the observed state of Check.
type CheckStatus struct {
	// checkID is the Pingdom check ID.
	// +optional
	CheckID *int64 `json:"checkID,omitempty"`

	// observedGeneration is the spec generation last applied to Pingdom.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// appliedHash is a hash of the request last sent to Pingdom, including Secret values.
	// A change, for example a rotated token, triggers a new update.
	// +optional
	AppliedHash string `json:"appliedHash,omitempty"`

	// lastSyncTime is when the check was last sent to Pingdom. Older than the resync interval triggers an
	// update that reverts changes made in the Pingdom UI.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// conditions represent the current state of the Check resource.
	//
	// The "Ready" condition is True when the check in Pingdom matches the spec.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=".spec.host"
// +kubebuilder:printcolumn:name="ID",type=integer,JSONPath=".status.checkID"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"

// Check is the Schema for the checks API
type Check struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Check
	// +required
	Spec CheckSpec `json:"spec"`

	// status defines the observed state of Check
	// +optional
	Status CheckStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CheckList contains a list of Check
type CheckList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Check `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Check{}, &CheckList{})
		return nil
	})
}
