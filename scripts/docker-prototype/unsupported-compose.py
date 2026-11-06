#!/usr/bin/env python3
"""Observe excluded features on macOS; never claim universal API rejection."""
import json
import pathlib
import platform
import subprocess
import tempfile

assert platform.system() == 'Darwin'
results = {}
with tempfile.TemporaryDirectory(prefix='pulse-bind-boundary-') as tmp:
    root = pathlib.Path(tmp)
    marker = 'primary-only-'+root.name
    for kind in ('host-volume','file-secret','file-config','driver-bind'):
        fixture = root/kind
        fixture.write_text(marker)
        service = {'image':'alpine:3.22','command':['cat','/fixture']}
        config = {'services':{'probe':service}}
        if kind == 'host-volume':
            service['volumes'] = [str(fixture)+':/fixture:ro']
        elif kind in ('file-secret','file-config'):
            key = 'secrets' if kind == 'file-secret' else 'configs'
            config[key] = {'fixture':{'file':str(fixture)}}
            service[key] = [{'source':'fixture','target':'/fixture'}]
        else:
            service['volumes'] = ['disguised:/fixture:ro']
            config['volumes'] = {'disguised':{'driver':'local','driver_opts':{'type':'none','o':'bind','device':str(fixture)}}}
        path = root/'compose.json'
        path.write_text(json.dumps(config))
        args = ['docker','compose','-p','pulse-negative-'+kind,'-f',str(path)]
        try:
            up = subprocess.run(args+['up','-d'], capture_output=True,text=True,timeout=90)
            if up.returncode == 0:
                ids = subprocess.check_output(args+['ps','-aq'],text=True).split()
                if ids:
                    subprocess.run(['docker','wait',*ids],capture_output=True,check=True,timeout=30)
            logs = subprocess.run(args+['logs','--no-color'],capture_output=True,text=True,timeout=30)
            assert marker not in logs.stdout+logs.stderr, 'unexpected macOS filesystem exposure'
            results[kind] = {'up_exit':up.returncode,'primary_marker_visible':False,
                'observation': 'daemon rejected setup' if up.returncode else 'container ran without primary file contents',
                'diagnostic':(up.stderr+logs.stdout+logs.stderr)[-3000:]}
        finally:
            subprocess.run(args+['down','-v','--remove-orphans'],capture_output=True,check=True,timeout=60)
print(json.dumps(results,indent=2))
