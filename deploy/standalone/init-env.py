#!/usr/bin/env python3
"""Generate host-local credentials once; never print their values."""
import base64, os, secrets, socket
from pathlib import Path
p=Path('.env')
if p.exists():
    raise SystemExit('Existing .env preserved; refusing credential rotation')
os.umask(0o077)
password=secrets.token_urlsafe(32)
token='pmc_'+secrets.token_hex(4)+'_'+secrets.token_urlsafe(32)
auth='Basic '+base64.b64encode(('monitoring:'+password).encode()).decode()
host_name=socket.gethostname()
p.write_text('HOST_NAME='+host_name+'\nPARSEABLE_HOSTNAME='+host_name+'\nPARSEABLE_PASSWORD='+password+'\nPARSEABLE_AUTH='+auth+'\nDAEMON_TOKEN='+token+'\n')
p.chmod(0o600)
print('Host-local credentials generated in .env (0600)')
