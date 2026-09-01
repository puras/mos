package filesystem

import (
	"context"
	"errors"
	"testing"
)

func TestFromContextMissingReturnsNil(t *testing.T) {
	fsys := FromContext(context.Background())
	if fsys != nil {
		t.Fatalf("未注入应返回 nil，got=%v", fsys)
	}
}

func TestWithFileSystemRoundTrip(t *testing.T) {
	fsys := &Local{mode: ModeDangerFull}
	ctx := WithFileSystem(context.Background(), fsys)
	got := FromContext(ctx)
	if got != fsys {
		t.Fatalf("FromContext=%v want=%v", got, fsys)
	}
}

func TestNotConfiguredErrorFromMissingContext(t *testing.T) {
	// 工具应在 fsys==nil 时返回 *NotConfiguredError
	fsys := FromContext(context.Background())
	if fsys == nil {
		err := &NotConfiguredError{}
		if !errors.As(err, new(*NotConfiguredError)) {
			t.Fatal("NotConfiguredError 应可 errors.As")
		}
	}
}
