#!/usr/bin/env python3
"""Disposable native-host TCP bridge. Payload remains end-to-end authenticated SSH.

Lab lifecycle: start detached on the worker, explicitly stop after VM cleanup.
Production must supervise/reconcile this service independently of Runtime.Close.
"""
import argparse
import json
import os
import signal
import socket
import threading

p = argparse.ArgumentParser()
p.add_argument('--target', required=True)
p.add_argument('--ready', required=True)
a = p.parse_args()
host, port = a.target.rsplit(':', 1)
listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
listener.bind(('0.0.0.0', 0))
listener.listen(128)
active = set()
lock = threading.Lock()


def stop(*_):
    listener.close()
    with lock:
        for conn in active:
            conn.close()
    os._exit(0)


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
if os.path.exists(a.ready):
    raise RuntimeError('endpoint state already exists')
with open(a.ready+'.tmp', 'x') as f:
    json.dump({'helper_kind':'pulse-test-worker-endpoint', 'port':listener.getsockname()[1], 'pid':os.getpid(), 'script':os.path.realpath(__file__), 'target':a.target, 'ready':os.path.abspath(a.ready)}, f)
os.replace(a.ready+'.tmp', a.ready)


def forward(local):
    remote = None
    try:
        remote = socket.create_connection((host, int(port)), 10)
        remote.settimeout(None)
        with lock:
            active.update((local, remote))
        def pump(src, dst):
            try:
                while True:
                    data = src.recv(65536)
                    if not data:
                        break
                    dst.sendall(data)
            except OSError:
                pass
            try:
                dst.shutdown(socket.SHUT_WR)
            except OSError:
                pass
        t = threading.Thread(target=pump, args=(local, remote), daemon=True)
        t.start()
        pump(remote, local)
    finally:
        local.close()
        if remote:
            remote.close()
        with lock:
            active.discard(local)
            active.discard(remote)


while True:
    local, _ = listener.accept()
    threading.Thread(target=forward, args=(local,), daemon=True).start()
