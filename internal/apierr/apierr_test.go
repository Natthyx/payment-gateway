package apierr

import (
	"errors"
	"net/http"
	"testing"
)

func TestConstructors(t *testing.T) {
	cases := []struct {
		e    *Error
		code Code
		st   int
	}{
		{InvalidRequest("x"), CodeInvalidRequest, http.StatusBadRequest},
		{Unauthorized("x"), CodeUnauthorized, http.StatusUnauthorized},
		{NotFound("x"), CodeNotFound, http.StatusNotFound},
		{Conflict("x"), CodeConflict, http.StatusConflict},
		{Provider("x"), CodeProviderError, http.StatusBadGateway},
		{Internal("x"), CodeInternal, http.StatusInternalServerError},
		{IdempotencyConflict("x"), CodeIdempotencyConflict, http.StatusConflict},
	}
	for _, c := range cases {
		if c.e.Code != c.code || c.e.Status != c.st {
			t.Errorf("got %+v", c.e)
		}
	}
}

func TestErrorsAs(t *testing.T) {
	e := InvalidRequest("oops")
	var got *Error
	if !errors.As(e, &got) {
		t.Fatal("expected As to succeed")
	}
	if got.Message != "oops" {
		t.Fatalf("msg=%q", got.Message)
	}
}
