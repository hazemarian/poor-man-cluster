#!/usr/bin/env python3
"""Run as tarek on Deshli. Preserve public login/state; replace only pmcluster units."""
import os,shlex,socket,sqlite3,subprocess,time,urllib.request,urllib.error,json,tempfile
from pathlib import Path
assert socket.gethostname()=='deshli', 'This integration is scoped to Deshli'
os.umask(0o077)
home=Path.home();root=home/'nextrum-public-console'
release='nextrum-observer-20261017-public';bins=root/'releases'/release
backup=Path((root/'last-backup').read_text().strip())
for name in ['edge.env','data.db','pmcluster-ui.db','pmcluster-daemon.service','pmcluster-edge.service']:
 assert (backup/name).is_file(), 'Missing backup '+name
assert release in subprocess.check_output([str(bins/'pmcluster'),'--version'],text=True)
bus={**os.environ,'XDG_RUNTIME_DIR':f'/run/user/{os.getuid()}','DBUS_SESSION_BUS_ADDRESS':f'unix:path=/run/user/{os.getuid()}/bus'}
def system(*args):return subprocess.check_output(['systemctl','--user',*args],env=bus,text=True).strip()
pid=system('show','pmcluster-edge.service','--property=MainPID','--value')
oldenv=dict(x.split('=',1) for x in Path('/proc/'+pid+'/environ').read_bytes().decode().split('\0') if '=' in x)
assert oldenv.get('PMCLUSTER_API_TOKEN') and oldenv.get('PMCLUSTER_UI_PASS')
monitor=dict(x.split('=',1) for x in (home/'nextrum-monitoring/.env').read_text().splitlines() if '=' in x)
def request(url,token=None):
 r=urllib.request.Request(url,headers={'Authorization':token} if token else {})
 with urllib.request.urlopen(r,timeout=8) as x:return x.status,x.read().decode()
assert request('http://127.0.0.1:18080/api/v1/about',monitor['PARSEABLE_AUTH'])[0]==200
def atomic_write(path,text,mode):
 with tempfile.NamedTemporaryFile(mode='w',dir=path.parent,delete=False) as f:
  temp=Path(f.name)
  try:
   os.fchmod(f.fileno(),mode);f.write(text);f.flush();os.fsync(f.fileno())
  except BaseException:
   temp.unlink(missing_ok=True);raise
 try:os.replace(temp,path)
 finally:temp.unlink(missing_ok=True)
edge_env=home/'selfhost/pmcluster/edge.env'
try:
 changes={'PMCLUSTER_OBSERVER_MODE':'true','EDGE_LOGIN_DISABLED':'false','OBSERVABILITY_PARSEABLE_URL':'http://127.0.0.1:18080','OBSERVABILITY_PARSEABLE_AUTH':monitor['PARSEABLE_AUTH'],'PMCLUSTER_PUBLIC_ORIGIN':'https://pmcluster.deshli.site','APP_VERSION':release}
 edge_env=home/'selfhost/pmcluster/edge.env'
 lines=[line for line in edge_env.read_text().splitlines() if line.removeprefix('export ').split('=',1)[0] not in changes]
 lines.extend(key+'='+shlex.quote(value) for key,value in changes.items())
 atomic_write(edge_env,'\n'.join(lines)+'\n',0o600)
 observer={'PMCLUSTER_OBSERVE_TOKEN':oldenv['PMCLUSTER_API_TOKEN'],'PMCLUSTER_DATA_DIR':str(home/'.pmcluster'),'PMCLUSTER_LISTEN_ADDR':'127.0.0.1:9090','PMCLUSTER_OTLP_ENDPOINT':''}
 atomic_write(root/'observer.env','\n'.join(k+'='+shlex.quote(v) for k,v in observer.items())+'\n',0o600)
 for name,envfile,binary,args in [('daemon',root/'observer.env',bins/'pmcluster',' observe'),('edge',edge_env,bins/'edge','')]:
  script=root/('start-'+name+'.sh')
  atomic_write(script,'#!/bin/sh\nset -eu\nset -a\n. '+shlex.quote(str(envfile))+'\nset +a\nexec '+shlex.quote(str(binary))+args+'\n',0o700)
  unit=home/'.config/systemd/user'/('pmcluster-'+name+'.service')
  text=(backup/unit.name).read_text()
  text='\n'.join('ExecStart='+str(script) if line.startswith('ExecStart=') else 'WorkingDirectory='+str(root) if line.startswith('WorkingDirectory=') else line for line in text.splitlines())+'\n'
  atomic_write(unit,text,0o644)
 system('daemon-reload');system('restart','pmcluster-daemon.service')
 token='Bearer '+oldenv['PMCLUSTER_API_TOKEN']
 for i in range(30):
  try:
   status,body=request('http://127.0.0.1:9090/api/nodes',token)
   nodes=json.loads(body)['nodes']
   assert status==200 and len(nodes)==1 and nodes[0]['role']=='standalone' and nodes[0]['availability']=='unknown'
   break
  except Exception:
   if i==29:raise
   time.sleep(1)
 system('restart','pmcluster-edge.service')
 for i in range(30):
  try:
   assert request('http://172.19.0.1:8042/web/login')[0]==200
   break
  except Exception:
   if i==29:raise
   time.sleep(1)
 class NoRedirect(urllib.request.HTTPRedirectHandler):
  def redirect_request(self,*args,**kwargs):return None
 try:
  urllib.request.build_opener(NoRedirect()).open('http://172.19.0.1:8042/web/observability',timeout=10)
  raise AssertionError('Anonymous console access was not denied')
 except urllib.error.HTTPError as e:
  assert e.code==302 and e.headers.get('Location','').startswith('/web/login'), 'Login gate changed'
 print('Activated',release,'with existing login/state/token and standalone host inventory')
 print('daemon_version',json.loads(request('http://127.0.0.1:9090/health')[1])['version'])
 print('units',system('is-active','pmcluster-daemon.service','pmcluster-edge.service'))
except Exception:
 # Restore service/config entrypoints; migrations are additive, databases kept.
 atomic_write(edge_env,(backup/'edge.env').read_text(),0o600)
 for name in ['pmcluster-daemon.service','pmcluster-edge.service']:
  atomic_write(home/'.config/systemd/user'/name,(backup/name).read_text(),0o644)
 system('daemon-reload');system('restart','pmcluster-daemon.service','pmcluster-edge.service')
 raise
