package main

import (
	"strings"
	"testing"
)

func TestAttendanceMessage(t *testing.T) {
	_, one := attendanceMessage(modeIn, []attEntry{{name: "小红", tm: "07:42"}})
	if !strings.HasPrefix(one, "小红 已于 07:42 到园（准时）") {
		t.Fatal(one)
	}
	_, two := attendanceMessage(modeIn, []attEntry{{name: "小红", tm: "07:42"}, {name: "小明", tm: "07:42"}})
	if !strings.HasPrefix(two, "小红、小明 已于 07:42 到园（准时）") {
		t.Fatal(two)
	}
	_, mixed := attendanceMessage(modeIn, []attEntry{{name: "小红", tm: "07:42"}, {name: "小明", tm: "07:45", late: true}})
	if !strings.HasPrefix(mixed, "小红 07:42（准时）、小明 07:45（迟到） 已到园") {
		t.Fatal(mixed)
	}
	_, out := attendanceMessage(modeOut, []attEntry{{name: "小红", tm: "16:10"}, {name: "小明", tm: "16:10"}})
	if !strings.HasPrefix(out, "小红、小明 已于 16:10 离园") {
		t.Fatal(out)
	}
}

func TestPhotoMessage(t *testing.T) {
	one := photoMessage(&photoBatch{order: []string{"小红"}, counts: map[string]int{"小红": 3}})
	if !strings.HasPrefix(one, "老师上传了 3 张 小红 的照片") {
		t.Fatal(one)
	}
	two := photoMessage(&photoBatch{order: []string{"小红", "小华"}, counts: map[string]int{"小红": 3, "小华": 2}})
	if !strings.HasPrefix(two, "新照片：小红 3 张、小华 2 张") {
		t.Fatal(two)
	}
}
