package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/CodisLabs/codis/pkg/utils/bufio2"
	"github.com/CodisLabs/codis/pkg/utils/bytesize"
	"github.com/CodisLabs/codis/pkg/utils/log"
	"github.com/CodisLabs/codis/pkg/utils/sync2/atomic2"

	"github.com/CodisLabs/redis-port/pkg/libs/pipe"
	"github.com/CodisLabs/redis-port/pkg/rdb"
)

func main() {
	const usage = `
Usage:
	redis-sync [--ncpu=N] (--master=MASTER|MASTER) --target=TARGET [--targetprefix=TP] [--db=DB] [--tmpfile-size=SIZE [--tmpfile=FILE]]
	redis-sync  --version

Options:
	-n N, --ncpu=N                    Set runtime.GOMAXPROCS to N.
	-m MASTER, --master=MASTER        The master redis instance ([auth@]host:port).
	-t TARGET, --target=TARGET        The target redis instance ([auth@]host:port).
	--targetprefix=TP                 Add prefix in target redis key.
	--db=DB                           Accept db = DB, default is *.
	--tmpfile=FILE                    Use FILE to as socket buffer.
	--tmpfile-size=SIZE               Set FILE size. If no --tmpfile is provided, a temporary file under current folder will be created.

Examples:
	$ redis-sync -m 127.0.0.1:6379 -t 127.0.0.1:6380
	$ redis-sync    127.0.0.1:6379 -t passwd@127.0.0.1:6380
	$ redis-sync    127.0.0.1:6379 -t passwd@127.0.0.1:6380 --db=0
	$ redis-sync    127.0.0.1:6379 -t passwd@127.0.0.1:6380 --db=0 --tmpfile-size=10gb
	$ redis-sync    127.0.0.1:6379 -t passwd@127.0.0.1:6380 --db=0 --tmpfile-size=10gb --tmpfile ~/sockfile.tmp
`
	var flags = parseFlags(usage)

	log.Debugf("parseFlags result:%+v", flags);

	var master struct {
		Path       string
		Addr, Auth string
		net.Conn
		rd *bufio2.Reader
		wt *bufio2.Writer

		rdb, aof struct {
			forward, skip atomic2.Int64
		}
		rbytes atomic2.Int64
	}
	// 解析 Source 链接
	master.Path = flags.Source
	if len(master.Path) == 0 {
		log.Panicf("invalid master address")
	}
	master.Addr, master.Auth = redisParsePath(master.Path)
	if len(master.Addr) == 0 {
		log.Panicf("invalid master address")
	}

	// 解析 target 链接
	var target struct {
		Path       string
		Addr, Auth string
	}
	target.Path = flags.Target
	if len(target.Path) == 0 {
		log.Panicf("invalid target address")
	}
	target.Addr, target.Auth = redisParsePath(target.Path)
	if len(target.Addr) == 0 {
		log.Panicf("invalid target address")
	}
	log.Infof("sync: master = %q, target = %q\n", master.Path, target.Path)

	// 打开临时文件
	var tmpfile *os.File
	if flags.TmpFile.Size != 0 {
		if flags.TmpFile.Path != "" {
			tmpfile = openReadWriteFile(flags.TmpFile.Path)
		} else {
			tmpfile = openTempFile(".", "tmpfile-")
		}
		defer closeFile(tmpfile)
	}

	// 建立master连接
	master.Conn = openConn(master.Addr, master.Auth)
	defer master.Close()
	master.rd = rBuilder(master.Conn).
		Buffer2(ReaderBufferSize).Reader.(*bufio2.Reader)
	master.wt = wBuilder(master.Conn).
		Buffer2(WriterBufferSize).Writer.(*bufio2.Writer)

	// 发送PSYNC命令开始主从同步，获取rdb文件
	var runid, offset, rdbSizeChan = redisSendPsyncFullsync(master.rd, master.wt)
	var rdbSize = func() int64 {
		for {
			select {
			case n := <-rdbSizeChan:
				if n != 0 {
					return n
				}
				log.Info("+")
			case <-time.After(time.Second):
				log.Info("-")
			}
		}
	}()
	// 完成获取rdb文件，打印结果
	log.Infof("sync: runid = %q, offset = %d", runid, offset)
	log.Infof("sync: rdb file = %d (%s)\n", rdbSize,
		bytesize.Int64(rdbSize).HumanString())

	var dumpoff atomic2.Int64
	var reploff = atomic2.Int64(offset)

	// 获取完rdb文件，开始通过pipe mode 读取aof文件
	var pipeReader = func() pipe.Reader {
		var mp = pipe.NewPipe()
		go func() {
			defer mp.Close()
			var psync = &struct {
				net.Conn
				rd *bufio2.Reader
				wt *bufio2.Writer
			}{
				master.Conn,
				master.rd, master.wt,
			}
			ioCopyN(wBuilder(mp.Writer()).Count(&dumpoff).Writer, psync.rd, rdbSize)

			for {
				var fence = NewJob(func() {
					defer psync.Conn.Close()
					io.Copy(wBuilder(mp.Writer()).Count(&reploff).Writer, psync.rd)
				}).Run()

				NewJob(func() {
					defer psync.Conn.Close()
					for {
						if err := redisSendReplAckNoCheck(psync.wt, reploff.Int64()); err != nil {
							log.WarnErrorf(err, "send replconf failed")
							return
						}
						time.Sleep(time.Second)
					}
				}).RunAndWait()

				<-fence

				log.Infof("connection lost %q", master.Addr)

				// 重连
			try_again:
				time.Sleep(time.Second)
				c, err := net.Dial("tcp", master.Addr)
				if err != nil {
					log.WarnErrorf(err, "cannot connect to %q", master.Addr)
					goto try_again
				} else {
					log.Infof("reconnect to %q", master.Addr)
				}
				psync.Conn = authenticate(c, master.Auth)
				psync.rd = rBuilder(psync.Conn).
					Buffer2(ReaderBufferSize).Reader.(*bufio2.Reader)
				psync.wt = wBuilder(psync.Conn).
					Buffer2(WriterBufferSize).Writer.(*bufio2.Writer)
				redisSendPsyncContinue(psync.rd, psync.wt, runid, reploff.Int64())
			}
		}()
		if tmpfile == nil {
			return mp.Reader()
		} else {
			var fp = pipe.NewPipeFile(tmpfile, int(flags.TmpFile.Size))
			go func() {
				defer fp.Close()
				ioCopyBuffer(fp.Writer(), mp.Reader())
			}()
			return fp.Reader()
		}
	}()
	defer pipeReader.Close()

	var reader = rBuilder(pipeReader).Must().Count(&master.rbytes).
		Buffer2(ReaderBufferSize).Reader.(*bufio2.Reader)

	var entryChan = newRDBLoader(io.LimitReader(reader, rdbSize), 32)

	// 这里是先解析rdb文件
	var jobs = NewParallelJob(flags.Parallel, func() {
		doRestoreDBEntry(entryChan, flags.TargetPrefix, target.Addr, target.Auth,
			func(e *rdb.DBEntry) bool {
				if !acceptDB(e.DB) {
					master.rdb.skip.Incr()
					return false
				}
				master.rdb.forward.Incr()
				return true
			})
	}).Then(func() {
		// 前面解析完rdb文件后，重放aof文件命令
		doRestoreAoflog(reader, flags.TargetPrefix, target.Addr, target.Auth,
			// 下面这个函数是过滤器，过滤掉 PING 命令
			func(db uint64, cmd string) bool {
				if !acceptDB(db) && cmd != "PING" {
					master.aof.skip.Incr()
					return false
				}
				master.aof.forward.Incr()
				return true
			})
	}).Run()

	log.Infof("sync: (r/f,s/f,s) = (read,rdb.forward,rdb.skip/rdb.forward,rdb.skip)")

	// 下面这里就是定时美妙打印进度信息。
	NewJob(func() {
		var last, stats struct {
			rdb, aof struct {
				forward, skip int64
			}
			dumpoff, reploff, rbytes int64
		}
		for stop := false; !stop; {
			select {
			case <-jobs:
				stop = true
			case <-time.After(time.Second):
			}
			stats.dumpoff = dumpoff.Int64()
			stats.reploff = reploff.Int64()
			stats.rbytes = master.rbytes.Int64()
			stats.rdb.forward = master.rdb.forward.Int64()
			stats.rdb.skip = master.rdb.skip.Int64()
			stats.aof.forward = master.aof.forward.Int64()
			stats.aof.skip = master.aof.skip.Int64()

			var b bytes.Buffer
			var percent float64
			if rdbSize != 0 {
				percent = float64(stats.dumpoff) * 100 / float64(rdbSize)
			}
			fmt.Fprintf(&b, "sync: rdb = %d - [%6.2f%%]", rdbSize, percent)
			fmt.Fprintf(&b, "   (r/f,s/f,s)=%s",
				formatAlign(4, "(%d/%d,%d/%d,%d)", stats.rbytes,
					stats.rdb.forward, stats.rdb.skip,
					stats.aof.forward, stats.aof.skip))
			fmt.Fprintf(&b, "  ~  %s",
				formatAlign(4, "(%s/-,-/-,-)",
					bytesize.Int64(stats.rbytes).HumanString()))
			fmt.Fprintf(&b, "  ~  speed=%s",
				formatAlign(4, "(%s/%d,%d/%d,%d)",
					bytesize.Int64(stats.rbytes-last.rbytes).HumanString(),
					stats.rdb.forward-last.rdb.forward, stats.rdb.skip-last.rdb.skip,
					stats.aof.forward-last.aof.forward, stats.aof.skip-last.aof.skip))
			last = stats
			log.Info(b.String())
		}
	}).RunAndWait()

	log.Info("sync: done")
}
