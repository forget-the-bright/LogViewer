package procmgr

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// flushInterval 定时 flush 间隔：平衡实时性与 WebSocket 消息数。
const flushInterval = 40 * time.Millisecond

// flushMaxLines 单批最大行数。静态模式突发大批数据时，攒满即 flush（不等定时器），
// 取较大值以减少大文件下的 WebSocket 消息数；实时模式下由定时器保证 40ms 延迟。
const flushMaxLines = 512

// Process 抽象一个可被 Manager 管控的进程：既可是本机 *exec.Cmd（localProc），
// 也可是远程 SSH 会话（sshProc，阶段二实现）。Manager 只依赖这个接口，
// 不关心进程运行在哪里。
type Process interface {
	StdoutPipe() (io.Reader, error)
	StderrPipe() (io.Reader, error)
	Start() error
	// Kill 终止进程及其子进程/进程组（本机杀进程组，远程发信号+关会话）。
	Kill() error
	Wait() error
}

// procRecord 封装一个正在运行的日志读取进程及其输出读取协程
type procRecord struct {
	id        uint64
	proc      Process
	done      chan struct{} // 进程退出且 stdout/stderr 读取协程全部结束后关闭
	readersWg sync.WaitGroup
	once      sync.Once
}

// Manager 统一管控所有子进程生命周期。
// 关键：任何退出场景（stop / ws断开 / 服务关闭）都必须调用 Stop/StopAll，
// 防止残留 tail / powershell 进程。
type Manager struct {
	mu     sync.Mutex
	procs  map[uint64]*procRecord
	nextID uint64
}

// NewManager 创建进程管理器
func NewManager() *Manager {
	return &Manager{procs: make(map[uint64]*procRecord)}
}

// Count 返回当前正在运行的进程数（供可观测性指标采样）。
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.procs)
}

// Start 启动进程并异步读取 stdout/stderr。
//
//	outFn: 收到一批 stdout 数据时回调（命令已完成过滤/编码，Go 只转发）
//	errFn: 收到 stderr 行时回调
//	doneFn: 进程结束且输出已全部 flush 完毕后回调（无论正常退出或被 kill）
func (m *Manager) Start(p Process, outFn func(string), errFn func(string), doneFn func()) (uint64, error) {
	stdout, err := p.StdoutPipe()
	if err != nil {
		return 0, err
	}
	stderr, err := p.StderrPipe()
	if err != nil {
		return 0, err
	}
	if err := p.Start(); err != nil {
		// StdoutPipe 已为进程分配资源（本机是管道，远程 SSH 是已 NewSession 的
		// channel，远端命令可能已经开始执行）。Start 失败时必须对称地 Kill 回收，
		// 否则远程 session/channel 泄漏，只能等 GC 或连接关闭。Kill 对本机
		// exec.Cmd（Process 尚为 nil）幂等无副作用。
		_ = p.Kill()
		return 0, err
	}

	m.mu.Lock()
	m.nextID++
	id := m.nextID
	rec := &procRecord{id: id, proc: p, done: make(chan struct{})}
	m.procs[id] = rec
	m.mu.Unlock()

	// stdout 读取协程：批量 + 独立定时协程 flush，避免 WebSocket 消息风暴
	rec.readersWg.Add(2)
	go m.readLoop(stdout, outFn, &rec.readersWg)
	// stderr 读取协程
	go m.readLines(stderr, errFn, &rec.readersWg)

	// 等待 & 清理协程：
	// 必须先等 stdout/stderr 读取协程结束（读到 EOF 并完成最后一次 flush），
	// 再调用 Wait() —— 这是 os/exec StdoutPipe 的标准用法，也保证 done
	// 关闭后不会再有 outFn/errFn 回调，杜绝"停止后仍向外写"。
	go func() {
		rec.readersWg.Wait()
		_ = p.Wait()
		rec.once.Do(func() { close(rec.done) })
		m.mu.Lock()
		if m.procs[id] == rec {
			delete(m.procs, id)
		}
		m.mu.Unlock()
		if doneFn != nil {
			doneFn()
		}
	}()
	return id, nil
}

// LocalProcess 把本机 *exec.Cmd 包装成 Process。
// Unix 下在 Start 前设置 Setpgid 以便 Kill 时连同整条管道一并终止；
// Windows 用 taskkill /T 终止进程树。平台差异在 procsys_*.go 中。
func LocalProcess(cmd *exec.Cmd) Process {
	return &localProc{cmd: cmd}
}

type localProc struct {
	cmd *exec.Cmd
}

func (l *localProc) StdoutPipe() (io.Reader, error) { return l.cmd.StdoutPipe() }
func (l *localProc) StderrPipe() (io.Reader, error) { return l.cmd.StderrPipe() }

func (l *localProc) Start() error {
	// Unix 下以独立进程组运行，便于 kill 时连同整条管道一并终止
	applyProcGroup(l.cmd)
	return l.cmd.Start()
}

func (l *localProc) Wait() error { return l.cmd.Wait() }

func (l *localProc) Kill() error {
	if l.cmd.Process == nil {
		return nil
	}
	// 先杀整个进程组/进程树（含 sh 的子进程 tail/grep/iconv，
	// 或 Windows 上 powershell 可能派生的子进程），再兜底 Kill。
	killGroupByPid(l.cmd.Process.Pid)
	return l.cmd.Process.Kill()
}

// readLoop 逐行读取并批量 flush。
//
// 关键设计：
//  1. flush 逻辑【不】放在阻塞 IO 的同一个循环里触发定时。
//     ReadString('\n') 在无新数据时会一直阻塞，若只在它返回后检查计时器，
//     首批到达的多行历史数据（tail -n N）会卡在 batch 里直到下一行到来。
//  2. 用独立的 ticker 协程周期性 flush，与读取协程解耦；batch 由 mu 保护。
//  3. outFn 由 flushMu 串行化，保证不会有两个 flush 并发写 WebSocket。
//  4. 退出时关闭 stopFlush 并等待 ticker 协程结束，确保 readLoop 返回后
//     绝无在途的 outFn 调用。
func (m *Manager) readLoop(r io.Reader, outFn func(string), wg *sync.WaitGroup) {
	defer wg.Done()

	br := bufio.NewReader(r)
	var mu sync.Mutex
	var flushMu sync.Mutex
	var batch []string

	flush := func() {
		mu.Lock()
		if len(batch) == 0 {
			mu.Unlock()
			return
		}
		text := strings.Join(batch, "")
		batch = batch[:0]
		mu.Unlock()
		// flushMu 串行化所有 outFn 调用（读取协程的满批 flush 与 ticker flush 不会并发）
		flushMu.Lock()
		defer flushMu.Unlock()
		if outFn != nil {
			outFn(text)
		}
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	stopFlush := make(chan struct{})
	var flushWg sync.WaitGroup
	flushWg.Add(1)
	go func() {
		defer flushWg.Done()
		for {
			select {
			case <-ticker.C:
				flush()
			case <-stopFlush:
				// 停止信号：先做最后一次 flush 由主循环负责，这里直接退出
				return
			}
		}
	}()

	for {
		line, err := br.ReadString('\n')
		if line != "" {
			mu.Lock()
			batch = append(batch, line)
			full := len(batch) >= flushMaxLines
			mu.Unlock()
			if full {
				flush() // 突发数据攒满即发，不等定时器，保证吞吐
			}
		}
		if err != nil {
			flush() // 读完/出错：把残余数据全部发出
			break
		}
	}

	// 通知 ticker 协程退出，并等待其可能在途的一次 flush 完成，
	// 此后不会再有任何 outFn 调用。
	close(stopFlush)
	flushWg.Wait()
}

// readLines 逐行读取 stderr
func (m *Manager) readLines(r io.Reader, errFn func(string), wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" && errFn != nil {
			errFn(line)
		}
		if err != nil {
			return
		}
	}
}

// Stop 停止指定会话的子进程（杀进程树 + 等待输出全部 flush + 回收）
func (m *Manager) Stop(id uint64) {
	m.mu.Lock()
	p, ok := m.procs[id]
	delete(m.procs, id)
	m.mu.Unlock()
	if !ok {
		return
	}
	// 杀进程组/进程树（localProc 内部杀 sh 的子进程 tail/grep/iconv，
	// 或 Windows powershell 可能派生的子进程）。
	_ = p.proc.Kill()
	// 等待进程退出 + 读取协程把最后一批数据 flush 完，最多等 1 秒。
	// Kill 是强杀，管道应很快收到 EOF；1s 足够兜底，避免停止操作长时间卡住前端。
	select {
	case <-p.done:
	case <-time.After(time.Second):
	}
}

// StopAll 停止所有子进程（服务关闭时调用）
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]uint64, 0, len(m.procs))
	for id := range m.procs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(id)
	}
}
