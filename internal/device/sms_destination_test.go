package device

import "testing"

func TestSMSDestinationForRegion(t *testing.T) {
	tests := []struct {
		name, input, region, want string
	}{
		{name: "chinese mobile", input: "13800138000", region: "CN", want: "+8613800138000"},
		{name: "british mobile", input: "07912345678", region: "GB", want: "+447912345678"},
		{name: "explicit international", input: "+447911123456", region: "CN", want: "+447911123456"},
		{name: "service code", input: "10086", region: "CN", want: "10086"},
		{name: "unknown region", input: "13800138000", want: "13800138000"},
		{name: "foreign number without prefix", input: "14165550123", region: "CN", want: "14165550123"},
		{name: "non numeric input", input: "138abc", region: "CN", want: "138abc"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := smsDestinationForRegion(test.input, test.region); got != test.want {
				t.Fatalf("destination = %q, want %q", got, test.want)
			}
		})
	}
}
