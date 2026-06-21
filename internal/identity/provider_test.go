package identity

import (
	"context"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// tokenReviewReactor installs a fake TokenReview response.
func withReview(status authnv1.TokenReviewStatus) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "tokenreviews", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, &authnv1.TokenReview{Status: status}, nil
	})
	return cs
}

func TestKubernetesSAProvider_Verify(t *testing.T) {
	ctx := context.Background()

	// Authenticated + pod-bound → Principal populated from token claims.
	cs := withReview(authnv1.TokenReviewStatus{
		Authenticated: true,
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:claw-agents:claw-agent-gcp-cost",
			Extra: map[string]authnv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"run-1-pod"},
				"authentication.kubernetes.io/pod-uid":  {"uid-1"},
			},
		},
	})
	p := &KubernetesSAProvider{Client: cs, Audience: "claw-controller"}
	pr, err := p.Verify(ctx, "tok")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if pr.Namespace != "claw-agents" || pr.SAName != "claw-agent-gcp-cost" || pr.PodName != "run-1-pod" || pr.PodUID != "uid-1" {
		t.Fatalf("principal = %+v", pr)
	}

	// Not authenticated → error.
	p2 := &KubernetesSAProvider{Client: withReview(authnv1.TokenReviewStatus{Authenticated: false, Error: "bad"})}
	if _, err := p2.Verify(ctx, "tok"); err == nil {
		t.Fatal("expected error for unauthenticated token")
	}

	// Authenticated but NOT pod-bound (no pod-uid) → refused.
	p3 := &KubernetesSAProvider{Client: withReview(authnv1.TokenReviewStatus{
		Authenticated: true,
		User:          authnv1.UserInfo{Username: "system:serviceaccount:claw-agents:claw-agent-gcp-cost"},
	})}
	if _, err := p3.Verify(ctx, "tok"); err == nil {
		t.Fatal("expected refusal for non-pod-bound token")
	}
}
