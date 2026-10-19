#!/usr/bin/env python3
"""Verify real public login without printing credentials or application log bodies."""
import os,subprocess,json,urllib.request,urllib.parse,urllib.error,http.cookiejar
from pathlib import Path
bus={**os.environ,'XDG_RUNTIME_DIR':f'/run/user/{os.getuid()}','DBUS_SESSION_BUS_ADDRESS':f'unix:path=/run/user/{os.getuid()}/bus'}
pid=subprocess.check_output(['systemctl','--user','show','pmcluster-edge.service','--property=MainPID','--value'],env=bus,text=True).strip()
env=dict(x.split('=',1) for x in Path('/proc/'+pid+'/environ').read_bytes().decode().split('\0') if '=' in x)
base='https://pmcluster.deshli.site'
class NoRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args,**kwargs):return None
try:
 urllib.request.build_opener(NoRedirect()).open(urllib.request.Request(base+'/web/observability',headers={'User-Agent':'Mozilla/5.0'}),timeout=20)
 raise AssertionError('Anonymous page was not denied')
except urllib.error.HTTPError as e:
 assert e.code==302 and e.headers.get('Location','').startswith('/web/login')
print('public_anonymous_login_gate_pass')
cookies=http.cookiejar.CookieJar();client=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies));client.addheaders=[('User-Agent','Mozilla/5.0')]
with client.open(base+'/web/login',timeout=20) as x:assert x.status==200
form=urllib.parse.urlencode({'username':env['PMCLUSTER_UI_USER'],'password':env['PMCLUSTER_UI_PASS']}).encode()
r=urllib.request.Request(base+'/web/login',data=form,headers={'Content-Type':'application/x-www-form-urlencoded','Origin':base})
with client.open(r,timeout=30) as x:
 body=x.read().decode();assert x.status==200 and 'data-nav="observability"' in body and '/web/login' not in x.url
assert 'href="/web/deploy"' not in body
assert 'The node list could not be read' not in body
print('existing_credentials_login_pass')
for path in ['/web/','/web/overview','/web/observability']:
 with client.open(base+path,timeout=30) as x:
  body=x.read().decode();assert x.status==200 and 'Telemetry evidence' in body and 'Ask the cluster' in body
  assert 'The node list could not be read' not in body
  assert '<pre class="inline-pre">' in body, 'No log evidence rows visible'
 print('public_page_pass',path)
with client.open(base+'/web/settings',timeout=30) as x:
 assert 'nextrum-observer-20261017-public' in x.read().decode()
print('public_settings_latest_version_pass')
for path in ['/api/nodes','/api/services']:
 r=urllib.request.Request(base+path,headers={'Authorization':'Bearer '+env['PMCLUSTER_API_TOKEN']})
 with client.open(r,timeout=20) as x:
  payload=json.load(x);assert x.status==200
 if path.endswith('nodes'):assert payload['nodes'][0]['role']=='standalone' and payload['nodes'][0]['availability']=='unknown'
 print('public_authenticated_api_pass',path)
with client.open(base+'/health',timeout=20) as x:
 version=json.load(x)['version'];assert version=='nextrum-observer-20261017-public'
print('public_live_version',version)
