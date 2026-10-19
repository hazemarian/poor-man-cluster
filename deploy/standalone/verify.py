#!/usr/bin/env python3
"""Run on the target from deployment directory. Prints no credentials/app bodies."""
import json,subprocess,urllib.request,urllib.error,time,datetime
from pathlib import Path
env=dict(line.split('=',1) for line in Path('.env').read_text().splitlines() if '=' in line)
def ip(service):
    cid=subprocess.check_output(['docker','compose','ps','-q',service],text=True).strip()
    o=json.loads(subprocess.check_output(['docker','inspect',cid],text=True))[0]
    return next(iter(o['NetworkSettings']['Networks'].values()))['IPAddress']
def req(base,path,token=None,obj=None,method=None,extra=None):
    headers={'Content-Type':'application/json',**(extra or {})}
    if token: headers['Authorization']=token
    r=urllib.request.Request(base+path,data=None if obj is None else json.dumps(obj).encode(),headers=headers,method=method)
    try:
        with urllib.request.urlopen(r,timeout=25) as x:return x.status,x.read().decode()
    except urllib.error.HTTPError as e:return e.code,e.read().decode()
backend='http://'+ip('parseable')+':8000';daemon='http://'+ip('daemon')+':9090'
auth=env['PARSEABLE_AUTH'];bearer='Bearer '+env['DAEMON_TOKEN']
status,body=req(daemon,'/api/services',bearer)
assert status==200,(status,body[:120])
services=json.loads(body)['services']
assert all(x['standalone'] and not x['desired_known'] for x in services)
print('inventory_services',len(services))
status,_=req(daemon,'/api/services');assert status==401,status
for path in ['/api/services/demo/web/restart','/api/services/demo/web/exec','/api/nodes/host/storage','/api/stacks/demo']:
    status,_=req(daemon,path,bearer,{},'POST');assert status==405,(path,status)
print('authorization_and_readonly_pass')
status,body=req('http://127.0.0.1:18042','/web/observability')
assert status==200,status
assert 'AI investigations are disabled' in body
print('native_page_http',status)
for stream in ['pmcluster-logs','pmcluster-metrics','pmcluster-traces']:
    # Pre-create absent streams so retention is set even before the first event.
    signal=stream.removeprefix('pmcluster-')
    status,_=req(backend,'/api/v1/logstream/'+stream,auth,method='PUT',extra={'X-P-Telemetry-Type':signal,'X-P-Log-Source':'otel-'+signal})
    assert status in (200,201,400),('stream',stream,status)
    status,_=req(backend,'/api/v1/logstream/'+stream+'/retention',auth,[{'duration':'1d','action':'delete','description':'Nextrum initial monitoring retention'}],'PUT')
    assert status in (200,201),(stream,status)
    print('retention_1day',stream)
now=datetime.datetime.now(datetime.timezone.utc)
def query(sql):
    deadline=time.monotonic()+60
    while True:
        current=datetime.datetime.now(datetime.timezone.utc)
        status,body=req(backend,'/api/v1/query',auth,{'query':sql,'startTime':(current-datetime.timedelta(hours=1)).isoformat(),'endTime':(current+datetime.timedelta(minutes=1)).isoformat()})
        if status==200:
            return json.loads(body)
        assert status==500 and 'p_timestamp' in body and time.monotonic()<deadline, ('query HTTP failure',status)
        time.sleep(3)
for stream in ['pmcluster-logs','pmcluster-metrics']:
    rows=query('SELECT COUNT(*) AS n FROM `'+stream+'`')
    print(stream,rows)
print('metric_types',query('SELECT metric_name, COUNT(*) AS n FROM `pmcluster-metrics` GROUP BY metric_name LIMIT 8'))
print('swarm',subprocess.check_output(['docker','info','--format','{{.Swarm.LocalNodeState}}'],text=True).strip())
deadline=time.monotonic()+90
while True:
    probe=query("SELECT COUNT(*) AS n FROM `pmcluster-logs` WHERE body LIKE '%NEXTRUM_LIVE_STDOUT_PROBE%' AND `service.name` = 'nextrum-check_probe'")
    host=query("SELECT COUNT(*) AS n FROM `pmcluster-metrics` WHERE metric_name LIKE 'system.%'")
    container=query("SELECT COUNT(*) AS n FROM `pmcluster-metrics` WHERE metric_name LIKE 'container.%'")
    status,body=req('http://127.0.0.1:18042','/web/observability?service=nextrum-check/probe')
    visible=status==200 and 'NEXTRUM_LIVE_STDOUT_PROBE' in body
    if probe[0]['n']>0 and host[0]['n']>0 and container[0]['n']>0 and visible:
        print('live_acceptance_pass', {'stdout_records':probe[0]['n'],'host_metric_records':host[0]['n'],'container_metric_records':container[0]['n'],'native_probe_visible':visible})
        break
    assert time.monotonic()<deadline, 'live stdout/host/container/native evidence acceptance timed out'
    time.sleep(5)
