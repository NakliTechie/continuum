#!/usr/bin/env python3
"""Real-binary terminal journey with disposable PTYs/state and a local fake app."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
CHILD = r"""
import os, select, signal, time, tty
signal.alarm(100)
tty.setraw(0)
os.write(1,b'\x1b[2;5H\x1b[6n')
reply=b''
while not reply.endswith(b'R'):
    reply+=os.read(0,32)
if reply!=b'\x1b[2;5R': os._exit(91)
os.write(1,b'\x1b[HPRIMARY_BASE\x1b[?1049h\x1b[31mAPP_READY\x1b[0m')
received=b''
last_size=os.get_terminal_size(0)
delayed_at=None
while True:
    size=os.get_terminal_size(0)
    if size!=last_size:
        last_size=size
        os.write(1,b'\x1b[2J\x1b[HAPP_READY RESIZE:'+str(size.columns).encode()+b'x'+str(size.lines).encode())
    if delayed_at is not None and time.monotonic()>=delayed_at:
        os.write(1,b'\x1b[8;1H\x1b[2KDETACHED_OUTPUT')
        delayed_at=None
    readable,_,_=select.select([0],[],[],.02)
    if readable:
        chunk=os.read(0,4096)
        received+=chunk
        os.write(1,b'\x1b[2;1H\x1b[2KRX:'+received.hex().encode())
        if b'\x15' in chunk:
            os.write(1,b'\x1b[5;1H\x1b[2KUNICODE:')
            for byte in 'e\u0301|\u754c|\U0001f469\U0001f3fd\u200d\U0001f4bb|\U0001f3f3\ufe0f\u200d\U0001f308'.encode():
                os.write(1,bytes([byte]));time.sleep(.006)
            os.write(1,b'\x1b[6;1H\x1b[2KUNICODE_DONE')
        if b'\x10' in chunk:
            os.write(1,b'\x1b[?1049l\r\nPRIMARY_RESTORED')
        if b'\x13' in chunk:
            for line in range(230):
                os.write(1,('SCROLL-%03d\r\n'%line).encode())
            os.write(1,b'SCROLL_END')
        if b'\x0e' in chunk:
            os.write(1,b'\x1b[?1049h\x1b[2J\x1b[HALT_REENTERED')
        if b'\x04' in chunk:
            delayed_at=time.monotonic()+.5
        if b'~' in chunk:
            os.write(1,b'\x1b[4;1HCHILD_DONE')
            os._exit(7)
"""

def wait_until(message, predicate, timeout=5):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        if predicate(): return
        time.sleep(.02)
    raise AssertionError(message)

class View:
    def __init__(self,binary,state,block,env,observer=False):
        self.master,self.slave=pty.openpty()
        self.resize(60 if observer else 80,15 if observer else 24)
        self.before=termios.tcgetattr(self.slave)
        self.flags=fcntl.fcntl(self.slave,fcntl.F_GETFL)
        args=[str(binary),'attach','--state',str(state),'--block',block]
        if observer:args.append('--observer')
        self.process=subprocess.Popen(args,env=env,stdin=self.slave,stdout=self.slave,stderr=self.slave,start_new_session=True)
        self.output=b''
        self.lock=threading.Lock();self.stopped=threading.Event()
        self.reader=threading.Thread(target=self.drain,daemon=True);self.reader.start()
    def resize(self,cols,rows):
        fcntl.ioctl(self.master,termios.TIOCSWINSZ,struct.pack('HHHH',rows,cols,0,0))
    def drain(self):
        while not self.stopped.is_set():
            if not select.select([self.master],[],[],.05)[0]:continue
            try:chunk=os.read(self.master,65536)
            except OSError:break
            if not chunk:break
            with self.lock:self.output=(self.output+chunk)[-2_000_000:]
    def poll(self):
        with self.lock:return self.output
    def send(self,data):os.write(self.master,data)
    def finish(self,code=0):
        wait_until('attach did not stop',lambda:self.process.poll() is not None)
        self.poll()
        assert self.process.returncode==code,(self.process.returncode,self.output[-1000:])
        # Darwin sets PENDIN when ICANON is restored. FIONREAD lets the kernel
        # settle that transition without consuming/discarding input; compare
        # every attribute afterwards, rather than masking any settings.
        fcntl.ioctl(self.slave,termios.FIONREAD,struct.pack('i',0))
        after=termios.tcgetattr(self.slave)
        assert after==self.before,('tty attributes were not restored',self.before,after)
        assert fcntl.fcntl(self.slave,fcntl.F_GETFL)&os.O_NONBLOCK==self.flags&os.O_NONBLOCK,'nonblocking input flag was not restored'
        assert b'\x1b[?1049l' in self.output,'alternate screen was not left'
    def close(self):
        if self.process.poll() is None:
            self.process.terminate()
            try:self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:self.process.kill();self.process.wait(timeout=3)
        self.stopped.set();self.reader.join(timeout=1)
        os.close(self.master);os.close(self.slave)

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary',type=Path,default=ROOT/'bin/continuum')
    parser.add_argument('--renewal',action='store_true',help='keep controller attached past its original 60-second lease')
    opts=parser.parse_args();binary=opts.binary.resolve()
    with tempfile.TemporaryDirectory(prefix='continuum-terminal-') as directory:
        root=Path(directory);state=root/'state'
        env={'HOME':str(root),'TMPDIR':str(root),'PATH':'/usr/bin:/bin','TERM':'xterm-256color'}
        views=[];daemon=None
        with (root/'daemon.log').open('wb') as log:
            def rpc(command,*args,code=0):
                r=subprocess.run([str(binary),command,'--state',str(state),'--json',*args],env=env,capture_output=True,timeout=12)
                assert r.returncode==code,(command,r.returncode,r.stdout,r.stderr)
                return json.loads(r.stdout)
            def screen():return rpc('screen','--block',block,'--observer')['result']
            def text():return ''.join(screen()['frame']['lines'])
            def start_daemon():
                process=subprocess.Popen([str(binary),'serve','--state',str(state)],env=env,stdout=log,stderr=log,start_new_session=True)
                def ready():
                    if process.poll() is not None:return False
                    if not (state/'v1.sock').exists():return False
                    try:
                        probe=subprocess.run([str(binary),'status','--state',str(state),'--json'],env=env,capture_output=True,timeout=2)
                    except subprocess.TimeoutExpired:
                        return False
                    return probe.returncode==0
                wait_until('daemon did not start',ready)
                return process
            try:
                daemon=start_daemon()
                status=rpc('status')['result']
                assert {'terminal_screen_v1','terminal_input_base64','control_renewal'}<=set(status['capabilities'])
                opened=rpc('open','--terminal','screen-v1','--cwd',str(root),'--',sys.executable,'-c',CHILD)['result']
                block=opened['block_id']
                time.sleep(.2) # child queries require an answer while all viewers are absent
                wait_until('detached query did not complete',lambda:'APP_READY' in text())
                controller=View(binary,state,block,env);views.append(controller)
                wait_until('controller did not render',lambda:b'APP_READY' in controller.poll())
                wait_until('application did not redraw after initial controller resize',lambda:'RESIZE:80x23' in text())
                observer=View(binary,state,block,env,True);views.append(observer)
                wait_until('observer did not render',lambda:b'Observer' in observer.poll())
                # Polling an idle revision emits no new outer-terminal bytes, so
                # local text selection is not destroyed by gratuitous repaints.
                time.sleep(.25);idle_bytes=len(controller.poll());time.sleep(.25)
                assert len(controller.poll())==idle_bytes,'idle frame repainted and disturbed client-owned selection'
                observer.send(b'bad')
                controller.send(b'\xe7')
                time.sleep(.12)
                controller.send(b'\x95\x8c\x03')
                wait_until('split Unicode/Ctrl-C input changed or observer wrote input',lambda:'RX:e7958c03' in text())
                controller.send(b'\x15')
                wait_until('complex Unicode output did not complete',lambda:'UNICODE_DONE' in text())
                unicode_text='e\u0301|\u754c|\U0001f469\U0001f3fd\u200d\U0001f4bb|\U0001f3f3\ufe0f\u200d\U0001f308'
                assert unicode_text in text(),'complex Unicode changed across byte-sized writes'
                assert screen()['frame']['alternate'],'application left alternate screen unexpectedly'
                controller.send(b'\x10')
                wait_until('primary screen was not restored',lambda:not screen()['frame']['alternate'] and 'PRIMARY_BASE' in text() and 'PRIMARY_RESTORED' in text())
                controller.send(b'\x13')
                wait_until('scrollback did not reach its bound',lambda:screen()['frame']['scrollback_lines']==200 and 'SCROLL_END' in text())
                controller.send(b'\x0e')
                wait_until('alternate screen could not be re-entered',lambda:screen()['frame']['alternate'] and 'ALT_REENTERED' in text())
                frame=screen()['frame'];assert (frame['cols'],frame['rows'])==(80,23),'observer resized process'
                controller.resize(50,10)
                wait_until('application did not recover after shrink',lambda:(screen()['frame']['cols'],screen()['frame']['rows'])==(50,9) and 'RESIZE:50x9' in text())
                controller.resize(100,31)
                wait_until('application did not recover after regrow',lambda:(screen()['frame']['cols'],screen()['frame']['rows'])==(100,30) and 'RESIZE:100x30' in text())
                controller.send(b'\x04');time.sleep(.15)
                observer.send(b'\x1d');observer.finish()
                controller.send(b'\x1d');controller.finish()
                wait_until('output stopped when all viewers detached',lambda:'DETACHED_OUTPUT' in text())
                assert rpc('status','--block',block)['result']['blocks'][0]['pid']==opened['pid'],'reconnect respawned the child'
                observer=View(binary,state,block,env,True);views.append(observer)
                wait_until('observer reconnect lost current screen',lambda:b'DETACHED_OUTPUT' in observer.poll())
                controller=View(binary,state,block,env);views.append(controller)
                wait_until('controller reconnect failed',lambda:b'APP_READY' in controller.poll())
                if opts.renewal:
                    print('CHECK: holding a real controller past its original 60-second lease',flush=True)
                    deadline=time.monotonic()+62
                    while time.monotonic()<deadline:
                        controller.poll();observer.poll()
                        assert controller.process.poll() is None,'controller stopped before renewal check'
                        time.sleep(.1)
                before_restart=screen()['frame']
                assert before_restart['epoch'],'screen epoch is missing'
                controller.send(b'\x04');time.sleep(.15)
                daemon.kill();daemon.wait(timeout=3)
                time.sleep(.5)
                daemon=start_daemon()
                wait_until('screen was not reconstructed from the holder ring',lambda:'DETACHED_OUTPUT' in text())
                after_restart=screen()['frame']
                assert after_restart['epoch']!=before_restart['epoch'],'daemon restart reused a volatile screen epoch'
                assert rpc('status','--block',block)['result']['blocks'][0]['pid']==opened['pid'],'daemon restart replaced the child'
                # Existing viewers stop at the explicit screen epoch boundary;
                # reconnecting creates fresh control/observation state.
                controller.finish(6);observer.finish(6)
                observer=View(binary,state,block,env,True);views.append(observer)
                wait_until('observer lost reconstructed screen after restart',lambda:b'DETACHED_OUTPUT' in observer.poll())
                controller=View(binary,state,block,env);views.append(controller)
                wait_until('controller could not reconnect after restart',lambda:b'DETACHED_OUTPUT' in controller.poll())
                controller.send(b'~')
                controller.finish(7);observer.finish(7)
                final=screen();assert final['state']=='exited' and final['exit_code']==7 and 'CHILD_DONE' in ''.join(final['frame']['lines'])
                print('PASS: detached queries, alternate/primary screens, idle selection stability, bounded scrollback, complex Unicode chunks, resize recovery, detach/reconnect, daemon-restart screen reconstruction and epoch fencing, same PID, final frame and exit status'+(', renewal beyond 60 seconds' if opts.renewal else ''),flush=True)
            except Exception:
                for index,view in enumerate(views):
                    print('VIEW_DIAGNOSTIC',index,view.process.poll(),repr(view.poll()[-2500:]),flush=True)
                raise
            finally:
                for view in views:view.close()
                if daemon is not None and daemon.poll() is None:
                    daemon.send_signal(signal.SIGTERM)
                    try:daemon.wait(timeout=5)
                    except subprocess.TimeoutExpired:os.killpg(daemon.pid,signal.SIGKILL);daemon.wait(timeout=3)
                if daemon is not None:assert daemon.returncode==0,'daemon did not shut down cleanly'

if __name__=='__main__':main()
