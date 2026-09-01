package assistant

import (
	"testing"

	fs "hegq.net/mos/filesystem"
)

func TestDefaultPolicyModeLocalJailed(t *testing.T) {
	p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: true, JailWrites: false}}
	if p.Mode() != fs.ModeLocalJailed {
		t.Errorf("want=local-jailed got=%s", p.Mode())
	}
}

func TestDefaultPolicyModeSaaSJailed(t *testing.T) {
	p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: true, JailWrites: true}}
	if p.Mode() != fs.ModeSaaSJailed {
		t.Errorf("want=saas-jailed got=%s", p.Mode())
	}
}

func TestDefaultPolicyModeDangerFull(t *testing.T) {
	p := &DefaultPolicy{FileAccess: FileAccessConfig{JailReads: false, JailWrites: false}}
	if p.Mode() != fs.ModeDangerFull {
		t.Errorf("want=danger-full got=%s", p.Mode())
	}
}
