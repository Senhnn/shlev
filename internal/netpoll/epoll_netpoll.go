package netpoll

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"runtime"
	"shlev/tools/logger"
	"shlev/tools/shleverror"
	"shlev/tools/task_queue"
	"sync/atomic"
)

type Epoller struct {
	epfd            int                       // epoll fd
	eventFd         int                       // EventFd用于通知重要事件
	eventFdBuf      []byte                    // EventFdBuffer
	wakeUpCall      int32                     // 是否通过紧急任务唤醒
	fdSet           map[int]struct{}          // epoll中的fd
	taskQueue       task_queue.AsyncTaskQueue // 低优先级任务队列
	urgentTaskQueue task_queue.AsyncTaskQueue // 高优先级任务队列
}

func NewEpoller() *Epoller {
	return &Epoller{}
}

func (e *Epoller) Open() (err error) {
	// unix.EPOLL_CLOEXEC
	// 当 flggs 为 0 时候，epoll_create1(0)与 epoll_create 功能一致
	/*如果设置为 EPOLL_CLOEXEC，那么由当前进程 fork 出来的任何子进程，其都会关闭其父进程的 epoll 实例所指向的文件描述符，
	也就是说子进程没有访问父进程 epoll 实例的权限。*/
	if e.epfd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC); err != nil {
		logger.Error("epoller open error! err:", err)
		err = os.NewSyscallError("epoll_create1", err)
		return err
	}
	if e.eventFd, err = unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC); err != nil {
		e.Close()
		if err = os.NewSyscallError("close", unix.Close(e.epfd)); err != nil {
			return err
		}

		logger.Error("Eventfd open error! err:", err)
		err = os.NewSyscallError("eventfd", err)
		return err
	}
	e.eventFdBuf = make([]byte, 8)
	if err = e.AddRead(e.eventFd); err != nil {
		_ = e.Close()
		logger.Error(fmt.Sprintf(""))
		return err
	}
	e.taskQueue = task_queue.NewLockFreeTaskQueue()
	e.urgentTaskQueue = task_queue.NewLockFreeTaskQueue()
	return nil
}

// Poll 阻塞当前协程，等待网络IO事件
func (e *Epoller) Polling(callback func(fd int, ev uint32) error) error {
	eventsList := newEventsList()
	// 是否执行任务
	var isExecTask bool

	for {
		n, err := unix.EpollWait(e.epfd, eventsList.events, 0)
		// unix.EINTR：这个调用被信号打断
		if n == 0 || (n < 0 && err == unix.EINTR) {
			runtime.Gosched()
			continue
		} else if err != nil {
			logger.Error(fmt.Sprintf("Poll error occurs in epoll: %v", os.NewSyscallError("epoll_wait", err)))
			return err
		}

		// 遍历返回的fd，处理事件
		for i := 0; i < n; i++ {
			ev := &eventsList.events[i]
			fd := int(ev.Fd)
			if fd != e.eventFd {
				err = callback(fd, ev.Events)
				switch err {
				case nil:
				case shleverror.ErrAcceptSocket, shleverror.ErrServerShutdown:
					logger.Error("Poll error:", err)
					return err
				default:
					logger.Warn("Poll other error:", err)
				}
			} else {
				isExecTask = true
				unix.Read(e.eventFd, e.eventFdBuf)
			}
		}

		// 处理完所有紧急任务
		if isExecTask {
			task := e.urgentTaskQueue.Dequeue()
			for task != nil {
				err = task.Run(task.Arg)
				switch err {
				case nil:
				case shleverror.ErrServerShutdown:
					logger.Error("Polling exec urgentTask error:", err)
					return err
				default:
					logger.Warn("Polling other error:", err)
				}
				task_queue.PutTask(task)
				task = e.urgentTaskQueue.Dequeue()
			}

			// 分片处理普通任务
			for i := 0; i < MaxTasksOnce; i++ {
				if task = e.taskQueue.Dequeue(); task == nil {
					break
				}
				err = task.Run(task.Arg)
				switch err {
				case nil:
				case shleverror.ErrServerShutdown:
					logger.Error("Poll exec task error:", err)
					return err
				default:
					logger.Warn("Poll other error:", err)
				}
				task_queue.PutTask(task)
			}
			atomic.StoreInt32(&e.wakeUpCall, 0)
			if (!e.taskQueue.IsEmpty() || !e.urgentTaskQueue.IsEmpty()) && atomic.CompareAndSwapInt32(&e.wakeUpCall, 0, 1) {
				_, err = unix.Write(e.eventFd, eventFdNtfData[:])
				switch err {
				case nil, unix.EAGAIN:
				default:
					isExecTask = true
				}
			}
		}
	}
}

// Close 关闭epoller
func (e *Epoller) Close() error {
	err := os.NewSyscallError("close", unix.Close(e.epfd))
	if err != nil {
		return err
	}
	return os.NewSyscallError("close", unix.Close(e.eventFd))
}

// AddReadWrite 注册fd的可读可写事件
func (e *Epoller) AddReadWrite(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readWriteEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("AddReadWrite epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl add", err)
	}

	e.fdSet[fd] = struct{}{}
	return nil
}

// AddRead 注册fd的可写时间
func (e *Epoller) AddRead(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("AddRead epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl add", err)
	}

	e.fdSet[fd] = struct{}{}
	return nil
}

// AddWrite 注册fd的可写事件
func (e *Epoller) AddWrite(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: writeEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("AddWrite epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl add", err)
	}

	e.fdSet[fd] = struct{}{}
	return nil
}

// ModRead 更新fd至可写事件
func (e *Epoller) ModRead(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("ModRead epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl mod", err)
	}

	return nil
}

// ModReadWrite 更新fd至可读可写事件
func (e *Epoller) ModReadWrite(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readWriteEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("ModReadWrite epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl mod", err)
	}

	return nil
}

// ModWrite 更新fd至可写事件
func (e *Epoller) ModWrite(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{Fd: int32(fd), Events: readWriteEvents})
	if err != nil {
		logger.Error(fmt.Sprintf("ModWrite epfd:%d add new fd:%d err", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl mod", err)
	}
	return nil
}

// Delete 从Epoller中删除fd
func (e *Epoller) Delete(fd int) error {
	err := unix.EpollCtl(e.epfd, unix.EPOLL_CTL_DEL, fd, nil)
	if err != nil {
		logger.Error(fmt.Sprintf("epfd:%d delete fd:%d error", e.epfd, fd))
		return os.NewSyscallError("epoll_ctl del", err)
	}
	delete(e.fdSet, fd)
	return nil
}

// GetFdNum 获取当前监听的fd数量
func (e *Epoller) GetFdNum() int {
	return len(e.fdSet)
}

// 用于给eventFd唤醒
var eventFdNtfData = [8]byte{0, 0, 0, 0, 0, 0, 0, 1}

// AddUrgentTask 把任务放入紧急队列中，然后唤醒正在等待的轮询器去执行任务
func (e *Epoller) AddUrgentTask(fn task_queue.TaskFunc, arg interface{}) (err error) {
	task := task_queue.GetTask()
	task.Run, task.Arg = fn, arg
	e.urgentTaskQueue.Enqueue(task)
	if atomic.CompareAndSwapInt32(&e.wakeUpCall, 0, 1) {
		if _, err = unix.Write(e.eventFd, eventFdNtfData[:]); err == unix.EAGAIN {
			err = nil
		}
	}
	return os.NewSyscallError("write", err)
}

// AddTask 将任务放入普通任务队列，优先级不如紧急任务队列高，在框架中用于发送消息给对端
func (p *Epoller) AddTask(fn task_queue.TaskFunc, arg interface{}) (err error) {
	task := task_queue.GetTask()
	task.Run, task.Arg = fn, arg
	p.taskQueue.Enqueue(task)
	if atomic.CompareAndSwapInt32(&p.wakeUpCall, 0, 1) {
		if _, err = unix.Write(p.eventFd, eventFdNtfData[:]); err == unix.EAGAIN {
			err = nil
		}
	}
	return os.NewSyscallError("write", err)
}
