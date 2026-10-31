package runtimecore

import "testing"

func TestDevIMEIDigitsComparesPlainAndURNForms(t *testing.T) {
	plain, urn := "860349050869734", "urn:gsma:imei:86034905-086973-4"
	if devIMEIDigits(plain, 14) != devIMEIDigits(urn, 14) || devIMEIDigits(plain, 8) != "86034905" {
		t.Fatalf("plain %q urn %q", devIMEIDigits(plain, 14), devIMEIDigits(urn, 14))
	}
	if devIMEIDigits("", 14) != "" || devIMEIDigits("8603", 8) != "" {
		t.Fatal("short IMEI was not reported as missing")
	}
}
