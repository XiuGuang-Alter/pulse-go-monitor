const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs/promises');
const os=require('node:os');
const path=require('node:path');
const {launchServer}=require('./launch-server.cjs');
test('startup timeout terminates the actual spawned process',async()=>{
 const dir=await fs.mkdtemp(path.join(os.tmpdir(),'pulse-launch-'));
 const pidFile=path.join(dir,'pid');
 try{
  const code="require('node:fs').writeFileSync(process.argv[1],String(process.pid));setInterval(()=>{},1000)";
  await assert.rejects(launchServer(process.execPath,['-e',code,pidFile],{timeout:2000}),/Startup timeout/);
  const pid=Number(await fs.readFile(pidFile,'utf8'));
  assert.throws(()=>process.kill(pid,0),{code:'ESRCH'});
 }finally{await fs.rm(dir,{recursive:true,force:true});}
});
test('spawn failure reports its original error',async()=>{
 await assert.rejects(launchServer(path.join(os.tmpdir(),'pulse-no-such-executable'),[]),{code:'ENOENT'});
});
