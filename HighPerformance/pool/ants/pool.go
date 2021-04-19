// MIT License

// Copyright (c) 2018 Andy Pan

// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package ants

import (
	"github.com/panjf2000/ants/v2/internal"
	"sync"
	"sync/atomic"
	"time"
)

// Pool 接收来自client的任务, 它用过一个给定的大小，限制总共的goroutine的数量，以实现循环使用goroutine
type Pool struct {
	// pool的容量， pool的容量为负值的话，代表pool的容量是没有限制的, 一个不限制大小的pool是用来，避免由池的嵌套使用引起的无限阻塞的潜在问题: 提交一个任务到pool中，提交一个新的任务到相同的pool中
	//submitting a task to pool which submits a new task to the same pool.
	capacity int32

	// running 当前运行的goroutine的数量
	running int32

	// workers 是一个用来存储可用的worker的切片
	workers workerArray

	// state 用来提示pool，它自己已经关闭
	state int32

	// lock 用来保证同步操作
	lock sync.Locker

	// cond 等待获取一个空闲的worker
	cond *sync.Cond

	// workerCache 加速了在function:retrieveWorker中获取一个可用的worker
	workerCache sync.Pool

	// blockingNum 是已经在pool.Submit处被阻塞的goroutine的数量, 被pool.lock保护
	blockingNum int

	//pool的配置：过期清理时间、是否需要预先分配内存、处理panic的处理器等
	options *Options
}

// purgePeriodically 定期清除过期的workers，它会单独运行一个goroutine作为清理者as a scavenger.
func (p *Pool) purgePeriodically() {
	// 定期
	heartbeat := time.NewTicker(p.options.ExpiryDuration)
	defer heartbeat.Stop()

	for range heartbeat.C {
		//pool是否已经关闭
		if p.IsClosed() {
			break
		}

		p.lock.Lock()
		//过期的workers
		expiredWorkers := p.workers.retrieveExpiry(p.options.ExpiryDuration)
		p.lock.Unlock()

		// Notify obsolete workers to stop.提醒过期的worker停止
		// This notification must be outside the p.lock, since w.task may be blocking and may consume a lot of time if many workers
		// are located on non-local CPUs.
		// 此通知必须在p.lock之外，因为w.task可能正在阻塞，并且如果有很多worker，可能会花费大量时间位于非本地CPU上
		for i := range expiredWorkers {
			//清除任务
			expiredWorkers[i].task <- nil
			expiredWorkers[i] = nil
		}

		// There might be a situation that all workers have been cleaned up(no any worker is running)
		// while some invokers still get stuck in "p.cond.Wait()",
		// then it ought to wakes all those invokers.
		//可能存在所有worker都被清理过的情况（没有任何worker在运行） 尽管某些调用程序仍然卡在“ p.cond.Wait（）”中， 那么它应该唤醒所有这些调用者。
		if p.Running() == 0 {
			//唤醒所有的调用者
			p.cond.Broadcast()
		}
	}
}

// NewPool 创建一个pool实例
func NewPool(size int, options ...Option) (*Pool, error) {
	opts := loadOptions(options...)
	// 没有限制的pool
	if size <= 0 {
		size = -1
	}

	if expiry := opts.ExpiryDuration; expiry < 0 {
		return nil, ErrInvalidPoolExpiry
	} else if expiry == 0 {
		// 使用默认的过期时间间隔
		opts.ExpiryDuration = DefaultCleanIntervalTime
	}
	// 使用默认的日志组件
	if opts.Logger == nil {
		opts.Logger = defaultLogger
	}

	p := &Pool{
		capacity: int32(size),
		lock:     internal.NewSpinLock(),
		options:  opts,
	}
	// sync.pool
	p.workerCache.New = func() interface{} {
		return &goWorker{
			pool: p,
			task: make(chan func(), workerChanCap), //任务的大小
		}
	}
	// 预先分配内存
	if p.options.PreAlloc {
		if size == -1 {
			return nil, ErrInvalidPreAllocSize
		}
		p.workers = newWorkerArray(loopQueueType, size)
	} else {
		p.workers = newWorkerArray(stackType, 0)
	}

	// 等待
	p.cond = sync.NewCond(p.lock)

	// 使用一个goroutine来清理过期的workers
	go p.purgePeriodically()

	return p, nil
}

// ---------------------------------------------------------------------------

// Submit 提交一个任务到pool中
func (p *Pool) Submit(task func()) error {
	if p.IsClosed() {
		return ErrPoolClosed
	}
	var w *goWorker
	// 获得一个可用的worker来运行任务
	if w = p.retrieveWorker(); w == nil {
		return ErrPoolOverload
	}
	// add task
	w.task <- task
	return nil
}

// Running 返回当前运行的goroutine的数量
func (p *Pool) Running() int {
	return int(atomic.LoadInt32(&p.running))
}

// Free 返回可用的goroutine的数量
func (p *Pool) Free() int {
	return p.Cap() - p.Running()
}

// Cap 返回pool的容量
func (p *Pool) Cap() int {
	return int(atomic.LoadInt32(&p.capacity))
}

// Tune 改变pool的容量， 这个方法对无限制大小的pool是没有作用的
func (p *Pool) Tune(size int) {
	// capacity == -1
	// size <= 0
	// p.options.PreAlloc 预分配了大小
	if capacity := p.Cap(); capacity == -1 || size <= 0 || size == capacity || p.options.PreAlloc {
		return
	}
	atomic.StoreInt32(&p.capacity, int32(size))
}

// IsClosed pool是否已经关闭
func (p *Pool) IsClosed() bool {
	return atomic.LoadInt32(&p.state) == CLOSED
}

// Release 关闭pool
func (p *Pool) Release() {
	//修改状态
	atomic.StoreInt32(&p.state, CLOSED)
	p.lock.Lock()
	p.workers.reset()
	p.lock.Unlock()
	// 这里可能有一些调用者等待在retrieveWorker()，所以我们需要唤醒他，以防这些调用者永久的阻塞
	p.cond.Broadcast()
}

// Reboot 重启一个已经释放的pool
func (p *Pool) Reboot() {
	if atomic.CompareAndSwapInt32(&p.state, CLOSED, OPENED) {
		go p.purgePeriodically()
	}
}

// ---------------------------------------------------------------------------

// incRunning 递增当前运行的goroutine的数量
func (p *Pool) incRunning() {
	atomic.AddInt32(&p.running, 1)
}

// decRunning 递减当前运行的goroutine的数量
func (p *Pool) decRunning() {
	atomic.AddInt32(&p.running, -1)
}

// retrieveWorker 返回一个可用的worker来运行任务
func (p *Pool) retrieveWorker() (w *goWorker) {
	// 获取一个worker
	spawnWorker := func() {
		// 从pool中获取一个可用的worker
		w = p.workerCache.Get().(*goWorker)
		w.run()
	}

	p.lock.Lock()

	w = p.workers.detach()

	if w != nil {
		p.lock.Unlock()
	} else if capacity := p.Cap(); capacity == -1 {
		// 不限制大小的pool
		p.lock.Unlock()
		spawnWorker()
	} else if p.Running() < capacity {
		//当前运行的goroutine的数量少于容量
		p.lock.Unlock()
		spawnWorker()
	} else {
		//如果是非阻塞的
		if p.options.Nonblocking {
			p.lock.Unlock()
			return
		}
	Reentry:
		if p.options.MaxBlockingTasks != 0 && p.blockingNum >= p.options.MaxBlockingTasks {
			p.lock.Unlock()
			return
		}
		// 阻塞
		p.blockingNum++
		// 加入等待队列
		p.cond.Wait()

		p.blockingNum--
		var nw int
		if nw = p.Running(); nw == 0 {
			p.lock.Unlock()
			if !p.IsClosed() {
				spawnWorker()
			}
			return
		}

		if w = p.workers.detach(); w == nil {
			if nw < capacity {
				p.lock.Unlock()
				spawnWorker()
				return
			}
			// 运行的goroutine的数量不小于capacity
			goto Reentry
		}

		p.lock.Unlock()
	}
	return
}

// revertWorker 将worker归还到pool中，重复使用goroutine
func (p *Pool) revertWorker(worker *goWorker) bool {
	if capacity := p.Cap(); (capacity > 0 && p.Running() > capacity) || p.IsClosed() {
		return false
	}
	worker.recycleTime = time.Now()
	p.lock.Lock()

	// 避免内存的泄漏，在锁范围内增加了一个双检测
	// Issue: https://github.com/panjf2000/ants/issues/113
	if p.IsClosed() {
		p.lock.Unlock()
		return false
	}

	err := p.workers.insert(worker)
	if err != nil {
		p.lock.Unlock()
		return false
	}

	// 提醒： 调用者卡在了'retrieveWorker()' of there is an available worker in the worker queue.
	p.cond.Signal()
	p.lock.Unlock()
	return true
}