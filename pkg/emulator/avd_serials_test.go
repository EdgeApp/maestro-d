package emulator

import "testing"

func TestRunningAVDsBySerial(t *testing.T) {
	scriptExec(t, func(name string, args []string) (string, bool) {
		if len(args) == 1 && args[0] == "devices" {
			return "List of devices attached\nemulator-5554\tdevice\nemulator-5556\tdevice\nemulator-5558\toffline\n" +
				"emulator-5560\tdevice\nR58M00TEST\tdevice\n\n", true
		}
		switch args[1] {
		case "emulator-5554":
			return "Pixel_8_API_35\n", true
		case "emulator-5556":
			return "202611080630-maestroDHub", true
		case "emulator-5560":
			return "", false // still booting
		}
		t.Errorf("unexpected call %v", args)
		return "", false
	})
	got := RunningAVDsBySerial()
	want := map[string]string{"emulator-5554": "Pixel_8_API_35", "emulator-5556": "202611080630-maestroDHub"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestRunningAVDsBySerial_EmptyNameAndNoAdb(t *testing.T) {
	scriptExec(t, func(name string, args []string) (string, bool) {
		if args[0] == "devices" {
			return "emulator-5554\tdevice\n", true
		}
		return "  \n", true
	})
	if got := RunningAVDsBySerial(); len(got) != 0 {
		t.Errorf("an empty avd_name is left out: %v", got)
	}
	scriptExec(t, func(string, []string) (string, bool) { return "", false })
	if got := RunningAVDsBySerial(); got == nil || len(got) != 0 {
		t.Errorf("adb failing gives an empty map: %#v", got)
	}
}
