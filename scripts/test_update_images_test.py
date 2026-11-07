import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('names', ROOT/'scripts/image-names.py')
names = importlib.util.module_from_spec(spec)
spec.loader.exec_module(names)

FAKE = r'''import copy,fcntl,json,os,sys,time
from pathlib import Path
state=Path(os.environ['FAKE_TART_STATE'])
args=sys.argv[1:]
tool=Path(sys.argv[0]).name
with open(str(state)+'.lock','w') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 d=json.loads(state.read_text());vms=d['vms'];d['calls'].append([tool,*args]);waiting=None;result='';code=0
 if tool=='tart':
  cmd=args[0]
  if cmd=='clone' and '--help' in args: result='--overwrite'
  elif cmd=='get':
   if args[1] not in vms: code=1
   else: result=json.dumps(vms[args[1]])
  elif cmd=='list': result=json.dumps(list(vms.values()))
  elif cmd=='clone':
   rest=[a for a in args[1:] if a!='--overwrite'];src,dst=rest
   if dst in vms and vms[dst]['State']=='running': code=1
   else:
    d['next']+=1
    vm=copy.deepcopy(vms.get(src,{'CPU':2,'Memory':4096,'State':'stopped','MACAddress':'raw','Release':'raw'}))
    vm.update(Name=dst,IP='192.0.2.'+str(d['next']));vms[dst]=vm
  elif cmd=='set':
   vm=vms[args[1]];vm.update(CPU=int(args[args.index('--cpu')+1]),Memory=int(args[args.index('--memory')+1]),MACAddress='fresh-'+vm['IP'])
  elif cmd=='run':
   waiting=args[-1];vms[waiting]['State']='running'
  elif cmd=='ip': result=vms[args[1]]['IP']
  elif cmd=='suspend': vms[args[1]]['State']='suspended'
  elif cmd=='stop': vms[args[1]]['State']='stopped'
  elif cmd=='delete': vms.pop(args[1],None)
  else: code=1
 elif tool=='ssh':
  address=next(a.split('@')[1] for a in args if '@192.0.2.' in a)
  vm=next(v for v in vms.values() if v['IP']==address)
  command=args[-1]
  if command=='true': pass
  elif command.startswith('mktemp'): result='/tmp/pulse-image-setup.TEST'
  elif 'prepare-base.sh' in command:
   vm.update(State='stopped',Release='new');result='PULSE_BASE_READY'
  elif 'prepare-template.sh' in command:
   if os.environ.get('FAIL_SLOT') and vm['Name'].endswith('-slot-'+os.environ['FAIL_SLOT']): code=1
   else: result='PULSE_TEMPLATE_READY {}'
  else: code=1
 elif tool=='scp': pass
 else: code=1
 state.write_text(json.dumps(d))
print(result)
if waiting:
 for _ in range(3000):
  with open(str(state)+'.lock','w') as lock:
   fcntl.flock(lock,fcntl.LOCK_EX)
   vm=json.loads(state.read_text())['vms'].get(waiting)
  if not vm or vm['State']!='running': break
  time.sleep(.01)
 else: sys.exit(2)
sys.exit(code)
'''

class UpdateImages(unittest.TestCase):
    def setUp(self):
        self.temp=TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);binary=self.root/'bin';binary.mkdir()
        self.sources=names.names('test','macos-26-arm64','standard',4,8192,2)
        vms={name:dict(Name=name,CPU=4,Memory=8192,State='stopped' if i==0 else 'suspended',MACAddress=f'old-{i}',Release='old',IP=f'192.0.2.{i+1}') for i,name in enumerate(self.sources)}
        # An already-running job clone must remain unchanged through publication.
        vms['old-job']=dict(vms[self.sources[1]],Name='old-job',State='running',IP='192.0.2.4')
        self.orchard='orchard-'+self.sources[1]+'-12345678-1234-1234-1234-123456789012-0'
        vms[self.orchard]=dict(vms[self.sources[1]],Name=self.orchard,IP='192.0.2.5')
        self.before=json.loads(json.dumps(vms))
        self.state=self.root/'state.json';self.state.write_text(json.dumps(dict(vms=vms,calls=[],next=10)))
        for tool in ('tart','ssh','scp'):
            path=binary/tool;path.write_text('#!'+sys.executable+'\n'+FAKE);path.chmod(0o755)
        uname=binary/'uname';uname.write_text('#!/bin/bash\nif [[ $1 == -s ]]; then echo Darwin; else echo arm64; fi\n');uname.chmod(0o755)
        self.env=dict(os.environ,PATH=str(binary)+os.pathsep+os.environ['PATH'],FAKE_TART_STATE=str(self.state),TART_HOME=str(self.root/'tart'),PULSE_IMAGE_LOG_DIR=str(self.root/'logs'))

    def run_update(self,*args):
        return subprocess.run(['bash',str(ROOT/'scripts/update-macos-images.sh'),'--owner','test','--slots','2','--c2j','a'*40,*args],env=self.env,capture_output=True,text=True,timeout=60)

    def test_publication_replaces_sources_and_keeps_running_clones(self):
        result=self.run_update();self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms']['old-job'],self.before['old-job'])
        for name in [*self.sources,self.orchard]: self.assertEqual(state['vms'][name]['Release'],'new')
        self.assertEqual(state['vms'][self.sources[1]]['MACAddress'],state['vms'][self.orchard]['MACAddress'])
        self.assertNotEqual(state['vms'][self.sources[1]]['MACAddress'],state['vms'][self.sources[2]]['MACAddress'])
        self.assertFalse(any(n.startswith('pulse-build-') for n in state['vms']))
        publishes=[i for i,c in enumerate(state['calls']) if c[:3]==['tart','clone','--overwrite']]
        suspends=[i for i,c in enumerate(state['calls']) if c[:2]==['tart','suspend']]
        self.assertGreater(min(publishes),max(suspends))

    def test_failed_preparation_does_not_publish_partial_sources(self):
        self.env['FAIL_SLOT']='2'
        result=self.run_update();self.assertNotEqual(result.returncode,0)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms'],self.before)
        self.assertFalse(any(c[:3]==['tart','clone','--overwrite'] for c in state['calls']))

    def test_ensure_does_not_build_or_update_existing_sources(self):
        result=self.run_update('--ensure');self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        state=json.loads(self.state.read_text())
        self.assertEqual(state['vms'],self.before)
        self.assertFalse(any(c[0]=='ssh' or (c[:2]==['tart','clone'] and '--help' not in c) for c in state['calls']))

    def test_alias_matching_does_not_select_jobs_or_other_pools(self):
        inventory=[{'Name':self.orchard},{'Name':'old-job'},{'Name':self.orchard+'-extra'},{'Name':self.orchard.replace('pulse-test-','pulse-other-')}]
        self.assertEqual(names.aliases(self.sources[1],inventory),[self.sources[1],self.orchard])


class DockerImageNames(unittest.TestCase):
    def test_docker_images_are_separate_and_version_independent(self):
        actual=names.names('mac-pool','macos-26-arm64','standard',4,8192,2,'engine')
        self.assertEqual(actual[0],'pulse-mac-pool-macos-26-arm64-docker-base')
        self.assertEqual(actual[1],'pulse-mac-pool-macos-26-arm64-standard-docker-engine-4cpu-8192m-slot-0001')
        self.assertEqual(len(set(actual)),3)

if __name__=='__main__': unittest.main()
