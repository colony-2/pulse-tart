#!/usr/bin/env python3
"""Two independent native Tart slots, two suspended generations each; no shares.

Runs full no-bind jobs and SDK/registry checks. Expect 15–30 minutes plus downloads.
Peak running allocation footprint: four VMs, 16 vCPUs, 32 GiB, plus host overhead.
The physical Mac must have capacity for this; do not run another suite concurrently.
"""
import json
import pathlib
import os
import urllib.request
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
if platform.system() != 'Darwin' or platform.machine() != 'arm64':
    sys.exit('Run on an Apple Silicon Mac with Tart installed.')
OUT = ROOT/'test-results'/(time.strftime('%Y%m%dT%H%M%SZ',time.gmtime())+'-docker-matrix-'+str(os.getpid()))
OUT.mkdir(parents=True)
shutil.copy2(__file__, OUT/"matrix-source.py")
children = []
report = {'scope':'native-two-suspended-slots','result':'incomplete','production_ready':False,'children':[]}
print('Starting two independent suspended slots; at most two running macOS VMs plus two Linux VMs / 16 vCPUs / 32 GiB. Full SDK checks run in each generation.',flush=True)
try:
    env = dict(os.environ)
    if env.get('PULSE_TEST_C2J_VERSION','latest') == 'latest':
        with urllib.request.urlopen('https://api.github.com/repos/colony-2/c2j/releases/latest',timeout=30) as response:
            env['PULSE_TEST_C2J_VERSION'] = json.load(response)['tag_name']
    report['c2j_selection'] = env['PULSE_TEST_C2J_VERSION']
    for slot in range(2):
        path=OUT/('slot-'+str(slot)+'.log')
        log=path.open('wb')
        args=[str(ROOT/'scripts/test-macos-docker.sh'),'--no-bind-platform','macos','--lifecycle','suspended','--rounds','2']
        process=subprocess.Popen(args,cwd=ROOT,env=env,stdout=log,stderr=subprocess.STDOUT)
        children.append((process,log,path))
    while any(p.poll() is None for p,_,_ in children):
        for i,(p,_,path) in enumerate(children):
            lines=path.read_text(errors='replace').splitlines()
            print('Slot',i,'running' if p.poll() is None else 'exited',lines[-1] if lines else 'building tools',flush=True)
        time.sleep(30)
    reports=[]
    for p,log,path in children:
        log.close()
        matches=re.findall(r'^Report: (test-results/[A-Za-z0-9T_-]+/report.json) Result: (\w+)$',path.read_text(errors='replace'),re.M)
        if not matches:
            raise RuntimeError('Child did not produce a report; inspect '+path.name)
        child_path=ROOT/matches[-1][0]
        data=json.loads(child_path.read_text())
        child = {'path':matches[-1][0],'exit':p.returncode,'result':data['result']}
        if data.get('run_error'):
            child['error'] = data['run_error']
            print('Slot',len(report['children']),'failed:',data['run_error'],flush=True)
            failure = re.search(r'command-failure-[a-f0-9]+\.log',data['run_error'])
            if failure:
                failure_path = child_path.parent/failure.group(0)
                if failure_path.is_file():
                    print(failure_path.read_text(errors='replace')[-4000:],flush=True)
        report['children'].append(child)
        archive=child_path.parent.with_suffix('.tar.gz')
        if archive.exists():shutil.copy2(archive,OUT/archive.name)
        reports.append(data)
    if any(child['exit'] or child['result'] != 'passed' for child in report['children']):
        raise RuntimeError('Child scope failed; see archived reports')
    pairs=[x for r in reports for x in r['pairs']]
    assert len(pairs)==4 and len({x['engine_id'] for x in pairs})==4, 'engine state reused'
    assert len({r['template']['mac'] for r in reports})==2, 'templates share a MAC'
    for key in ('host_key_sha256','client_key_sha256'):
        assert len({x['keys'][key] for x in pairs})==4, 'transport key reused'
    assert all(x.get('job_running_first_unix') and x.get('job_running_last_unix') for x in pairs), 'missing observed running intervals'
    overlap=max(min(a['job_running_last_unix'],b['job_running_last_unix'])-max(a['job_running_first_unix'],b['job_running_first_unix']) for a in reports[0]['pairs'] for b in reports[1]['pairs'])
    assert overlap>5, 'jobs did not overlap; concurrency not established'
    report.update(result='passed',overlap_seconds=overlap,distinct_engine_ids=4,distinct_template_macs=2,distinct_key_pairs=4)
except BaseException as error:
    report['error']=str(error)
    # Child scripts own their VMs. On interruption do not bypass their cleanup
    # with a global VM deletion or an unsafe name-prefix match.
    print('Matrix incomplete:',error,flush=True)
finally:
    for p,log,path in children:
        if p.poll() is None:
            print('Child still running:',p.pid,'log:',path.relative_to(ROOT),'; allow its owned cleanup to finish.',flush=True)
        log.close()
    (OUT/'report.json').write_text(json.dumps(report,indent=2)+'\n')
    with tarfile.open(str(OUT)+'.tar.gz','w:gz') as archive:
        archive.add(OUT,arcname=OUT.name)
    print('Report:',(OUT/'report.json').relative_to(ROOT),'Result:',report['result'],flush=True)
    print('Send:',str(OUT.relative_to(ROOT))+'.tar.gz',flush=True)
sys.exit(0 if report['result']=='passed' else 2)
