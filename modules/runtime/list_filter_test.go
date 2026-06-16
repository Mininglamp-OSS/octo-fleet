package runtime

import "testing"

// MAJOR1 (codex impl review): active provider 列表为空时,list 查询必须返回空,
// 而不是退化成跳过 IN 过滤、列出全部。空 active 的 guard 在任何 d.session 调用
// 之前 return,故可用 nil session 的 runtimeDB 直接单测。

func TestListBySpaceIDAndOwner_EmptyActiveReturnsEmpty(t *testing.T) {
	d := &runtimeDB{}
	got, err := d.listBySpaceIDAndOwner("space", "owner", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty active providers must yield empty list, got %d rows", len(got))
	}
}

func TestListBotsBySpace_EmptyActiveReturnsEmpty(t *testing.T) {
	d := &runtimeDB{}
	got, total, err := d.listBotsBySpace("space", "owner", "", nil, 1, 20)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Errorf("empty active kinds must yield empty bot list, got %d rows (total %d)", len(got), total)
	}
}
