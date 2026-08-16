package server

import "testing"

func TestClassifyStderr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means suppressed (benign)
	}{
		{"empty", "", ""},
		{"whitespace only", "  \n", ""},
		{"gnu appeared english", "tail: 'app.log' has appeared;  following new file", ""},
		{"gnu replaced english", "tail: 'app.log' has been replaced;  following new file", ""},
		{"gnu truncated english", "tail: 'app.log': file truncated", ""},
		{"gnu appeared chinese", "tail: '/wanghao/app.log' 已被建立；正在跟随新文件的末尾", ""},
		{"gnu truncated chinese", "tail: '/wanghao/app.log': 文件已截断", ""},
		{"real error no such file", "tail: cannot open 'missing' for reading: No such file or directory", "tail: cannot open 'missing' for reading: No such file or directory"},
		{"grep real error", "grep: repetition-operator operand invalid", "grep: repetition-operator operand invalid"},
		{"powershell error", "Select-String : Cannot bind argument", "Select-String : Cannot bind argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyStderr(c.in)
			if got != c.want {
				t.Errorf("classifyStderr(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
