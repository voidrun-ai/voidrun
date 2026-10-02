//go:build linux

package runtime

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type pidWatch struct {
	done chan struct{}
}

var (
	epollOnce sync.Once
	epollErr  error
	epfd      int

	watchMu sync.Mutex
	watches map[int]*pidWatch
)

func ensureEpoll() error {
	epollOnce.Do(func() {
		fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
		if err != nil {
			epollErr = err
			return
		}
		epfd = fd
		watches = make(map[int]*pidWatch)
		go epollLoop()
	})
	return epollErr
}

func takeWatch(fd int) *pidWatch {
	watchMu.Lock()
	defer watchMu.Unlock()
	w := watches[fd]
	delete(watches, fd)
	return w
}

func epollLoop() {
	events := make([]unix.EpollEvent, 64)
	for {
		n, err := unix.EpollWait(epfd, events, -1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Printf("[pidwatch] epoll: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			w := takeWatch(fd)
			if w == nil {
				continue
			}
			_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
			_ = unix.Close(fd)
			close(w.done)
		}
	}
}

func dropWatch(fd int) {
	if takeWatch(fd) == nil {
		return
	}
	_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
	_ = unix.Close(fd)
}

// LiveCHPID reads the sandbox pidfile and returns the pid if it is a live
// cloud-hypervisor process. ok is false if the file is missing, the pid is
// dead, or cmdline does not match CHBinary.
func LiveCHPID(id string) (pid int, ok bool) {
	data, err := os.ReadFile(GetPIDPath(id))
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return 0, false
	}
	if !pidMatchesCH(pid) {
		return 0, false
	}
	return pid, true
}

// WatchPID blocks until pid exits or stop is closed. It uses a shared epoll
// of pidfds, so it works for processes this process did not spawn.
func WatchPID(pid int, stop <-chan struct{}) error {
	if pid <= 0 {
		return unix.ESRCH
	}
	if err := ensureEpoll(); err != nil {
		return err
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if err == unix.ESRCH {
			return nil
		}
		return err
	}

	w := &pidWatch{done: make(chan struct{})}
	watchMu.Lock()
	watches[fd] = w
	watchMu.Unlock()

	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)}
	if err := unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &ev); err != nil {
		dropWatch(fd)
		return err
	}

	select {
	case <-w.done:
		return nil
	case <-stop:
		if takeWatch(fd) != nil {
			_ = unix.EpollCtl(epfd, unix.EPOLL_CTL_DEL, fd, nil)
			_ = unix.Close(fd)
			return ErrWatchStopped
		}
		<-w.done
		return nil
	}
}
