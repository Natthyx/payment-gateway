package outbound

import (
	"testing"

	"github.com/Natthyx/payment-gateway/internal/apierr"
)

func TestValidateEndpointURL(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		why string
	}{
		{"", false, "empty"},
		{"http://example.com", false, "non-https"},
		{"https://example.com/webhook", true, "ok"},
		{"ftp://example.com", false, "non-https"},
		{"https://", false, "no host"},
	}
	for _, c := range cases {
		err := validateEndpointURL(c.in)
		if c.ok && err != nil {
			t.Errorf("%s: expected ok, got %v", c.why, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected error", c.why)
		}
		if !c.ok && err != nil {
			// make sure it's a structured invalid_request
			ae, _ := err.(*apierr.Error)
			if ae == nil || ae.Code != apierr.CodeInvalidRequest {
				t.Errorf("%s: expected structured invalid_request error, got %v", c.why, err)
			}
		}
	}
}
