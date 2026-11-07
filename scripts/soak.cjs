// Opt-in synthetic experiment. This creates its own binary, database and upstream.
const fs=require('node:fs'),path=require('node:path'),http=require('node:http');
const {spawnSync}=require('node:child_process');
const {once}=require('node:events');
const {setTimeout:delay}=require('node:timers/promises');
const {performance}=require('node:perf_hooks');
const {launchServer,stopChild}=require('./launch-server.cjs');
const {SoakHealth}=require('./soak-health.cjs');
const {createHash}=require('node:crypto');
const root=path.resolve(__dirname,'..');
const duration=Number(process.env.PULSE_SOAK_SECONDS||7200);
if(!Number.isFinite(duration)||duration<1)throw Error('Invalid duration');
const dir=path.resolve(process.env.PULSE_SOAK_OUT||path.join(root,'.cache','soak-'+Date.now()));
if(!dir.startsWith(root+path.sep))throw Error('Evidence must stay in project');
fs.mkdirSync(dir,{recursive:true});
const csv=path.join(dir,'samples.csv');
fs.writeFileSync(csv,'utc,elapsed_s,samples,completed_per_second,running,heap_alloc_bytes,goroutines,rss_bytes,peak_upstream_active\n');
let app,upstream,base,active=0,peak=0,overlap=0,completed=0,previousElapsed=0,previousCompleted=0;
const health=new SoakHealth();let logs='';
const perURL=new Map(),counts=new Map(),seenKinds=new Set();
async function api(route,method='GET',data){const response=await fetch(base+route,{method,headers:data?{'Content-Type':'application/json'}:{},body:data?JSON.stringify(data):undefined,signal:AbortSignal.timeout(10000)});if(!response.ok)throw Error(method+' '+route+' HTTP'+response.status);return response.status===204?null:response.json();}
function input(url,i){return {name:'synthetic-'+i,url,interval_seconds:10,timeout_seconds:1,enabled:true,...(i===0?{rules:{json_equals:[{pointer:'/code',equals:0}]}}:{})};}
function rss(pid){if(process.platform!=='win32')return '';const shell=process.env.PULSE_PWSH||'pwsh';const result=spawnSync(shell,['-NoProfile','-Command',`(Get-Process -Id ${pid} -ErrorAction Stop).WorkingSet64`],{encoding:'utf8',windowsHide:true});if(result.status!==0)throw Error('RSS sampler failed');return Number(result.stdout.trim());}
async function main(){
 const executable=path.join(dir,process.platform==='win32'?'pulse-soak.exe':'pulse-soak');
 const bundled=path.join(root,'.tools/go/bin/go.exe');
 const go=fs.existsSync(bundled)?bundled:'go';
 const env={...process.env,GOCACHE:path.join(root,'.cache/build'),GOTELEMETRY:'off'};if(fs.existsSync(bundled))env.GOPATH=path.join(root,'.cache/gopath');
 const build=spawnSync(go,['build','-trimpath','-ldflags=-s -w','-buildvcs=false','-o',executable,'.'],{cwd:root,stdio:'inherit',env});if(build.status!==0)throw Error('Build failed');
 const sourceFiles=[...fs.readdirSync(root).filter(n=>n.endsWith('.go')&&!n.endsWith('_test.go')), 'go.mod','go.sum',...fs.readdirSync(path.join(root,'web')).map(n=>'web/'+n)].sort();
 const hash=createHash('sha256');for(const file of sourceFiles){hash.update(file+'\0');hash.update(fs.readFileSync(path.join(root,file)));}
 const sourceHash=hash.digest('hex');
 upstream=http.createServer((req,res)=>{
  const key=req.url;active++;peak=Math.max(peak,active);perURL.set(key,(perURL.get(key)||0)+1);if(perURL.get(key)>1)overlap++;
  let ended=false;const timer=setTimeout(()=>{if(key.startsWith('/slow/'))res.end('slow');else if(key.startsWith('/error/'))res.writeHead(500).end('error');else if(key.startsWith('/json/'))res.end('{"code":1}');else res.end('ok');},key.startsWith('/slow/')?2000:50);
  const end=()=>{if(ended)return;ended=true;clearTimeout(timer);active--;perURL.set(key,perURL.get(key)-1);};res.once('close',end);res.once('finish',end);
 });upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
 const upstreamBase='http://127.0.0.1:'+upstream.address().port;
 const args=['serve','-addr','127.0.0.1:0','-concurrency','4','-db',path.join(dir,'synthetic.db')];
 app=await launchServer(executable,args);base=app.url;app.child.stderr.on('data',chunk=>{logs+=chunk;if(logs.length>65536)logs=logs.slice(-65536);});
 let victim;
 for(let i=0;i<100;i++){const route=i===0?'json':i===1?'error':i===2?'slow':'ok';const target=await api('/api/targets','POST',input(upstreamBase+'/'+route+'/'+i,i));if(i===99)victim=target;}
 const start=performance.now();let nextMutation=300,nextRSS=0,lastRSS='';let mutations=0;
 fs.writeFileSync(path.join(dir,'environment.json'),JSON.stringify({source_sha256:sourceHash,started_utc:new Date().toISOString(),duration_seconds:duration,platform:process.platform,arch:process.arch,node:process.version,targets:100,concurrency:4,interval_seconds:10,pid:app.child.pid,database:'synthetic independent fixture'},null,2));
 while(true){
  const elapsed=(performance.now()-start)/1000;
  const [status,targets]=await Promise.all([api('/api/status'),api('/api/targets')]);
  health.observe(elapsed,targets,logs);
  // A cancelled request can remain visible to the Node upstream until its close
  // callback runs. Enforce Go's running slots; retain socket peak as diagnostics.
  if(targets.length!==100||status.running>4||overlap)throw Error('Correctness violation: '+JSON.stringify({targets:targets.length,running:status.running,overlap,upstream_socket_peak:peak}));
  for(const t of targets){const last=counts.get(t.id)||0;completed+=Math.max(0,t.stats.samples-last);counts.set(t.id,t.stats.samples);if(t.latest)seenKinds.add(t.latest.kind);}
  if(elapsed>=nextRSS){lastRSS=rss(app.child.pid);nextRSS=elapsed+60;}
  const rate=(completed-previousCompleted)/Math.max(.001,elapsed-previousElapsed);previousElapsed=elapsed;previousCompleted=completed;
  fs.appendFileSync(csv,[new Date().toISOString(),elapsed.toFixed(3),completed,rate.toFixed(3),status.running,status.heap_alloc_bytes,status.goroutines,lastRSS,peak].join(',')+'\n');
  if(elapsed>=duration)break;
  if(elapsed>=nextMutation){
   const oldID=victim.id;await api('/api/targets/'+oldID,'DELETE');const check=await fetch(base+'/api/targets/'+oldID+'/checks');if(check.status!==404)throw Error('Deleted target history revived');
   mutations++;victim=await api('/api/targets','POST',input(upstreamBase+'/ok/replacement-'+mutations,99));
   const editable=targets.find(t=>t.id!==oldID&&t.id!==1);await api('/api/targets/'+editable.id,'PUT',{name:'edited-'+mutations,url:editable.url,interval_seconds:10,timeout_seconds:1,enabled:true});
   nextMutation=elapsed+300;
  }
  await delay(Math.min(10000,Math.max(10,(duration-elapsed)*1000)));
 }
 if(duration>=60&&!['ok','http_error','timeout','assertion_failed'].every(k=>seenKinds.has(k)))throw Error('Missing mixed result kind');
 await stopChild(app.child);app=await launchServer(executable,args);base=app.url;
 const restored=await api('/api/targets');if(restored.length!==100)throw Error('Persistence after process restart failed');
 const result={passed:true,elapsed_seconds:(performance.now()-start)/1000,configured_seconds:duration,completed_samples:completed,peak_upstream_active:peak,same_url_overlaps:overlap,mutations,seen_kinds:[...seenKinds],restart_targets:restored.length,termination:'isolated child terminated and restarted; graceful SIGINT/Ctrl+C covered separately'};
 fs.writeFileSync(path.join(dir,'result.json'),JSON.stringify(result,null,2));console.log(JSON.stringify(result));
}
main().catch(error=>{fs.writeFileSync(path.join(dir,'failure.txt'),String(error.stack));console.error(error);process.exitCode=1;}).finally(async()=>{try{await stopChild(app?.child);}finally{if(upstream){upstream.closeAllConnections();await new Promise(resolve=>upstream.close(resolve));}}});
