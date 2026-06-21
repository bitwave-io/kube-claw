package identity

import (
	"context"
	"fmt"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Principal is the verified identity of a calling workload.
type Principal struct {
	ServiceAccount string // "system:serviceaccount:<ns>:<name>"
	Namespace      string
	SAName         string
	PodName        string // from the token's BOUND claims (not caller-supplied)
	PodUID         string
}

// Provider verifies a platform credential. Default: KubernetesSAProvider. Future
// impls (OIDC/SPIFFE/cloud IAM) satisfy the same interface (DESIGN.md §9).
type Provider interface {
	Verify(ctx context.Context, credential string) (Principal, error)
}

// KubernetesSAProvider verifies a projected ServiceAccount token via TokenReview
// and extracts the bound pod name/uid from the review's extra claims — so the
// pod identity comes from the token, never from a caller-supplied argument
// (closes the co-resident-pod replay gap, DESIGN.md §9 / outside-voice #1).
type KubernetesSAProvider struct {
	Client   kubernetes.Interface
	Audience string // expected token audience, e.g. "claw-controller"
}

func (p *KubernetesSAProvider) Verify(ctx context.Context, token string) (Principal, error) {
	tr := &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token},
	}
	if p.Audience != "" {
		tr.Spec.Audiences = []string{p.Audience}
	}
	res, err := p.Client.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return Principal{}, fmt.Errorf("tokenreview: %w", err)
	}
	if !res.Status.Authenticated {
		return Principal{}, fmt.Errorf("token not authenticated: %s", res.Status.Error)
	}

	u := res.Status.User
	ns := firstExtra(u.Extra, "authentication.kubernetes.io/credential-id") // unused; placeholder
	_ = ns
	pr := Principal{
		ServiceAccount: u.Username,
		PodName:        firstExtra(u.Extra, "authentication.kubernetes.io/pod-name"),
		PodUID:         firstExtra(u.Extra, "authentication.kubernetes.io/pod-uid"),
	}
	pr.Namespace, pr.SAName = parseSA(u.Username)
	if pr.PodUID == "" {
		return Principal{}, fmt.Errorf("token is not pod-bound (no pod-uid claim); refusing")
	}
	return pr, nil
}

func firstExtra(extra map[string]authnv1.ExtraValue, key string) string {
	if v, ok := extra[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// parseSA splits "system:serviceaccount:<ns>:<name>".
func parseSA(username string) (ns, name string) {
	const prefix = "system:serviceaccount:"
	if len(username) <= len(prefix) || username[:len(prefix)] != prefix {
		return "", ""
	}
	rest := username[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == ':' {
			return rest[:i], rest[i+1:]
		}
	}
	return "", ""
}
