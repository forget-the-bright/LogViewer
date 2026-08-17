package server

import (
	"strings"
	"testing"
)

// TestRingBufferEviction 验证环形缓冲按字节上限淘汰最旧批次，oldestSeq 正确前移。
func TestRingBufferEviction(t *testing.T) {
	sess := &viewSession{}
	// 每批 100 字节，容量 2MB => 可容纳约 20971 批。
	for i := uint64(1); i <= 100; i++ {
		sess.nextSeq = i + 1
		sess.pushRing(i, strings.Repeat("x", 100))
	}
	if sess.ringBytes != 100*100 {
		t.Fatalf("ringBytes = %d, want %d", sess.ringBytes, 100*100)
	}
	if sess.oldestSeq != 1 {
		t.Fatalf("oldestSeq = %d, want 1（未触发淘汰）", sess.oldestSeq)
	}

	// 灌入足够多数据触发淘汰（>2MB）。
	for i := uint64(101); i <= 30000; i++ {
		sess.nextSeq = i + 1
		sess.pushRing(i, strings.Repeat("x", 100))
	}
	if sess.ringBytes > sessionRingBytes {
		t.Fatalf("ringBytes = %d 超过上限 %d", sess.ringBytes, sessionRingBytes)
	}
	if sess.oldestSeq <= 1 {
		t.Fatalf("oldestSeq = %d，淘汰后应前移", sess.oldestSeq)
	}
	// 缓冲中所有 seq 都应 >= oldestSeq 且严格递增。
	prev := uint64(0)
	for _, e := range sess.ring {
		if e.seq < sess.oldestSeq {
			t.Fatalf("条目 seq=%d < oldestSeq=%d", e.seq, sess.oldestSeq)
		}
		if e.seq <= prev {
			t.Fatalf("seq 非递增: %d after %d", e.seq, prev)
		}
		prev = e.seq
	}
}

// TestEntriesAfter 验证按 lastSeq 补发与缺口检测。
func TestEntriesAfter(t *testing.T) {
	sess := &viewSession{}
	for i := uint64(1); i <= 10; i++ {
		sess.nextSeq = i + 1
		sess.pushRing(i, "line"+uitoa(i))
	}

	// lastSeq=5：补发 6..10，无缺口。
	got, gap := sess.entriesAfter(5)
	if gap {
		t.Errorf("gap=true, want false（lastSeq 仍在窗口内）")
	}
	if len(got) != 5 {
		t.Fatalf("补发 %d 条, want 5", len(got))
	}
	for j, e := range got {
		if want := uint64(6 + j); e.seq != want {
			t.Errorf("补发条目[%d].seq=%d, want %d", j, e.seq, want)
		}
	}

	// lastSeq=10（已读到最后）：无补发，无缺口。
	got, gap = sess.entriesAfter(10)
	if gap || len(got) != 0 {
		t.Errorf("lastSeq=10 应无补发: got=%d gap=%v", len(got), gap)
	}

	// 制造淘汰，使 oldestSeq 前移到 >3。
	for i := uint64(11); i <= 30000; i++ {
		sess.nextSeq = i + 1
		sess.pushRing(i, strings.Repeat("x", 100))
	}
	if sess.oldestSeq <= 3 {
		t.Fatalf("测试前置失败: oldestSeq=%d 应已 >3", sess.oldestSeq)
	}
	// lastSeq=3 落在被淘汰区间：应报告 gap，但仍补发窗口内全部条目。
	got, gap = sess.entriesAfter(3)
	if !gap {
		t.Errorf("gap=false, want true（lastSeq 已被淘汰）")
	}
	for _, e := range got {
		if e.seq < sess.oldestSeq {
			t.Errorf("补发了已淘汰条目 seq=%d < oldestSeq=%d", e.seq, sess.oldestSeq)
		}
	}
}

// TestUitoa 验证序号格式化。
func TestUitoa(t *testing.T) {
	cases := map[uint64]string{0: "0", 1: "1", 42: "42", 1234567890: "1234567890"}
	for in, want := range cases {
		if got := uitoa(in); got != want {
			t.Errorf("uitoa(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestRandHex 验证会话 ID 长度与随机性。
func TestRandHex(t *testing.T) {
	a := randHex(8)
	b := randHex(8)
	if len(a) != 16 { // 8 字节 => 16 个十六进制字符
		t.Fatalf("randHex(8) 长度 = %d, want 16", len(a))
	}
	if a == b {
		t.Fatalf("两次 randHex 结果相同: %s", a)
	}
}
