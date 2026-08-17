package server

import "testing"

func TestClassifyStderr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want stderrClass
	}{
		{"empty", "", stderrBenign},
		{"whitespace only", "  \n", stderrBenign},
		// 首次出现：良性，忽略
		{"gnu appeared english", "tail: 'app.log' has appeared;  following new file", stderrBenign},
		{"gnu appeared chinese", "tail: '/wanghao/app.log' 已被建立；正在跟随新文件的末尾", stderrBenign},
		// 轮转/替换：notice rotate
		{"gnu replaced english", "tail: 'app.log' has been replaced;  following new file", stderrRotate},
		{"gnu replaced chinese", "tail: '/wanghao/app.log' 已被替换；正在跟随新文件的末尾", stderrRotate},
		// 截断：notice truncate
		{"gnu truncated english", "tail: 'app.log': file truncated", stderrTruncate},
		{"gnu truncated chinese", "tail: '/wanghao/app.log': 文件已截断", stderrTruncate},
		// 真实错误
		{"real error no such file", "tail: cannot open 'missing' for reading: No such file or directory", stderrError},
		{"grep real error", "grep: repetition-operator operand invalid", stderrError},
		{"powershell error", "Select-String : Cannot bind argument", stderrError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyStderr(c.in)
			if got != c.want {
				t.Errorf("classifyStderr(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
