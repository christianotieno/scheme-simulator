package main

import (
	"errors"
	"strconv"
	"testing"
)

func TestParseRequestValid(t *testing.T) {
	for _, amount := range []int{1, 100, 101, 10000, 10001} {
		line := "PAYMENT|" + strconv.Itoa(amount)
		got, err := parseRequest(line)
		if err != nil {
			t.Fatalf("parseRequest(%q) err = %v; want nil", line, err)
		}
		if got != amount {
			t.Errorf("parseRequest(%q) amount = %d; want %d", line, got, amount)
		}
	}
}

func TestRejectReason(t *testing.T) {
	if got := rejectReason(errInvalidRequest); got != "Invalid request" {
		t.Errorf("rejectReason(errInvalidRequest) = %q; want %q", got, "Invalid request")
	}
	if got := rejectReason(errInvalidAmount); got != "Invalid amount" {
		t.Errorf("rejectReason(errInvalidAmount) = %q; want %q", got, "Invalid amount")
	}
}

// A leading '+' is accepted: "+100" is a positive integer. See DECISIONS.md.
func TestParseRequestLeadingPlus(t *testing.T) {
	got, err := parseRequest("PAYMENT|+100")
	if err != nil {
		t.Fatalf(`parseRequest("PAYMENT|+100") err = %v; want nil`, err)
	}
	if got != 100 {
		t.Errorf(`parseRequest("PAYMENT|+100") amount = %d; want 100`, got)
	}
}

func TestParseRequestMalformed(t *testing.T) {
	cases := map[string]string{
		"wrong prefix":       "INVALID|100",
		"lowercase prefix":   "payment|100",
		"empty prefix":       "|100",
		"missing separator":  "PAYMENT",
		"empty line":         "",
		"extra field":        "PAYMENT|100|extra",
		"trailing separator": "PAYMENT|100|",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(line)
			if !errors.Is(err, errInvalidRequest) {
				t.Errorf("parseRequest(%q) err = %v; want errInvalidRequest", line, err)
			}
		})
	}
}

func TestParseRequestInvalidAmount(t *testing.T) {
	cases := map[string]string{
		"zero":            "PAYMENT|0",
		"negative":        "PAYMENT|-5",
		"non-numeric":     "PAYMENT|abc",
		"decimal":         "PAYMENT|1.5",
		"leading space":   "PAYMENT| 100",
		"trailing space":  "PAYMENT|100 ",
		"empty amount":    "PAYMENT|",
		"overflow":        "PAYMENT|99999999999999999999",
		"carriage return": "PAYMENT|100\r", // parseRequest assumes framing already stripped
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseRequest(line)
			if !errors.Is(err, errInvalidAmount) {
				t.Errorf("parseRequest(%q) err = %v; want errInvalidAmount", line, err)
			}
		})
	}
}
