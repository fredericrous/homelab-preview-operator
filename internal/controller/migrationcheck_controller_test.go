package controller

import (
	"net/url"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	previewv1 "github.com/fredericrous/homelab-preview-operator/api/v1"
)

func TestBuildDatabaseURL_EncodesSpecialChars(t *testing.T) {
	// CNPG-generated passwords can contain URL-significant bytes (@ : / ? # %).
	pass := "p@ss:w/rd?#%x"
	got := buildDatabaseURL("postgres", pass, "migcheck-abc-rw.migration-check-x.svc.cluster.local", "ticket-vision")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("DSN must be a parseable URL, got %q: %v", got, err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme = %q, want postgres", u.Scheme)
	}
	if u.User.Username() != "postgres" {
		t.Errorf("user = %q, want postgres", u.User.Username())
	}
	if p, _ := u.User.Password(); p != pass {
		t.Errorf("password round-trip = %q, want %q", p, pass)
	}
	if u.Host != "migcheck-abc-rw.migration-check-x.svc.cluster.local:5432" {
		t.Errorf("host = %q", u.Host)
	}
	if u.Path != "/ticket-vision" {
		t.Errorf("path = %q, want /ticket-vision", u.Path)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Errorf("sslmode = %q, want disable", u.Query().Get("sslmode"))
	}
	// Raw form must not leak un-encoded specials into the userinfo.
	if strings.Contains(got, "p@ss") {
		t.Errorf("password not encoded in raw DSN: %q", got)
	}
}

func TestMigCheckID_StableAndDNSSafe(t *testing.T) {
	mc := &previewv1.MigrationCheck{ObjectMeta: metav1.ObjectMeta{UID: types.UID("3f2504e0-4f89-41d3-9a0c-0305e82c3301")}}
	a := migCheckID(mc)
	b := migCheckID(mc)
	if a != b {
		t.Errorf("migCheckID not stable: %q vs %q", a, b)
	}
	if len(a) != 12 {
		t.Errorf("migCheckID len = %d, want 12", len(a))
	}
	if strings.ContainsAny(a, "-_.") {
		t.Errorf("migCheckID has non-DNS chars: %q", a)
	}
	// Different UID -> different id (no PR-number collision).
	mc2 := &previewv1.MigrationCheck{ObjectMeta: metav1.ObjectMeta{UID: types.UID("00000000-1111-2222-3333-444444444444")}}
	if migCheckID(mc2) == a {
		t.Errorf("distinct UIDs produced same id")
	}
}

func TestEndpointsReady(t *testing.T) {
	empty := &corev1.Endpoints{}
	if endpointsReady(empty) {
		t.Error("empty Endpoints reported ready")
	}
	notReady := &corev1.Endpoints{Subsets: []corev1.EndpointSubset{{NotReadyAddresses: []corev1.EndpointAddress{{IP: "1.2.3.4"}}}}}
	if endpointsReady(notReady) {
		t.Error("not-ready-only Endpoints reported ready")
	}
	ready := &corev1.Endpoints{Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "1.2.3.4"}}}}}
	if !endpointsReady(ready) {
		t.Error("ready Endpoints reported not ready")
	}
}
