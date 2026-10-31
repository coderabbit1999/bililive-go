package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckCSVHeader(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"精确匹配", strings.TrimSuffix(csvHead, "\n") + "\n", false},
		{"带 BOM", "\xEF\xBB\xBF" + csvHead, false},
		{"CRLF 行尾", strings.TrimSuffix(csvHead, "\n") + "\r\n", false},
		{"无结尾换行", strings.TrimSuffix(csvHead, "\n"), false},
		{"旧表头", "ts,ts_unix,room_id\n", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("header-%d.csv", i))
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			err := checkCSVHeader(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkCSVHeader 返回 %v，期望 wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestSampleRecordOrder(t *testing.T) {
	offset := int64(42)
	audience := int64(7)
	s := Sample{
		TS:         "2026-01-02 03:04:05",
		TSUnix:     123,
		OffsetSec:  &offset,
		RoomID:     "100",
		Host:       "主播",
		Title:      "标题",
		LiveStatus: 1,
		Popularity: 2,
		Watched:    3,
		Likes:      4,
		Audience:   &audience,
		Recording:  true,
	}
	want := map[string]string{
		"ts":          "2026-01-02 03:04:05",
		"ts_unix":     "123",
		"offset_s":    "42",
		"room_id":     "100",
		"host":        "主播",
		"title":       "标题",
		"live_status": "1",
		"popularity":  "2",
		"watched":     "3",
		"likes":       "4",
		"recording":   "1",
		"audience":    "7",
	}

	record := sampleRecord(s)
	if len(record) != len(csvColumns) {
		t.Fatalf("列数 %d，期望 %d", len(record), len(csvColumns))
	}
	for i, column := range csvColumns {
		if record[i] != want[column] {
			t.Errorf("第 %d 列 %s = %q，期望 %q", i, column, record[i], want[column])
		}
	}
}

func TestWriteRowErrorAndClear(t *testing.T) {
	m := &Manager{}
	if err := m.appendSample(Sample{TS: "t", TSUnix: 1, RoomID: "1"}); err == nil {
		t.Fatal("writer 为 nil 时应返回错误")
	}
	if got := m.Snapshot().OutputError; got == "" {
		t.Fatal("落盘失败后 OutputError 不应为空")
	}

	m.output = filepath.Join(t.TempDir(), "viewer_log.csv")
	if err := m.openOutput(); err != nil {
		t.Fatal(err)
	}
	defer m.closeOutput()

	if err := m.appendSample(Sample{TS: "t", TSUnix: 1, RoomID: "1"}); err != nil {
		t.Fatalf("写入应成功: %v", err)
	}
	if got := m.Snapshot().OutputError; got != "" {
		t.Fatalf("写入恢复后 OutputError 应为空，实际 %q", got)
	}
}

func TestEffectiveAudienceInterval(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     time.Duration
	}{
		{0, audienceInterval},
		{3 * time.Second, 15 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{15 * time.Second, 15 * time.Second},
		{20 * time.Second, 20 * time.Second},
		{25 * time.Second, 25 * time.Second},
	}
	for _, tc := range cases {
		m := &Manager{interval: tc.interval}
		if got := m.effectiveAudienceInterval(); got != tc.want {
			t.Errorf("interval=%s 时得到 %s，期望 %s", tc.interval, got, tc.want)
		}
	}
}

func TestParseRoomID(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://live.bilibili.com/21763044", "21763044"},
		{"https://live.bilibili.com/21763044?x=1", "21763044"},
		{"https://live.bilibili.com/21763044/#/", "21763044"},
		{"https://example.com/123", ""},
		{"https://live.bilibili.com/blanc/123", ""},
	}
	for _, tc := range cases {
		if got := parseRoomID(tc.url); got != tc.want {
			t.Errorf("parseRoomID(%q) = %q，期望 %q", tc.url, got, tc.want)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name    string
		page    string
		want    string
		wantErr bool
	}{
		{"直接对象", `STATE={"a":1}`, `{"a":1}`, false},
		{"嵌套对象", `STATE={"a":{"b":2}}`, `{"a":{"b":2}}`, false},
		{"字符串内花括号", `STATE={"a":"}{"}`, `{"a":"}{"}`, false},
		{"转义引号", `STATE={"a":"x\"}y"}`, `{"a":"x\"}y"}`, false},
		{"缺少标记", `no marker here`, "", true},
		{"未闭合", `STATE={"a":1`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractJSONObject([]byte(tc.page), "STATE=")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v，期望 wantErr=%v", err, tc.wantErr)
			}
			if string(got) != tc.want {
				t.Errorf("得到 %q，期望 %q", string(got), tc.want)
			}
		})
	}
}
