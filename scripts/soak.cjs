// Independent synthetic experiment. This never resumes or overwrites an old run.
const fs=require('node:fs'),path=require('node:path'),http=require('node:http');
const {spawnSync}=require('node:child_process');
const {once}=require('node:events');
const {setTimeout:delay}=require('node:timers/promises');
const {performance}=require('node:perf_hooks');
const {createHash}=require('node:crypto');
const {launchServer,stopChild}=require('./launch-server.cjs');
const {SoakHealth}=require('./soak-health.cjs');
const root=path.resolve(__dirname,'..');
function positiveEnv(name,fallback){const value=Number(process.env[name]||fallback);if(!Number.isFinite(value)||value<1)throw Error('Invalid '+name);return value;}
const duration=positiveEnv('PULSE_SOAK_SECONDS',7200);
const mutationSeconds=positiveEnv('PULSE_SOAK_MUTATION_SECONDS',300);
const dir=path.resolve(process.env.PULSE_SOAK_OUT||path.join(root,'.cache','soak-'+Date.now()));
function within(parent,target){const relative=path.relative(parent,target);return relative!==''&&!relative.startsWith('..'+path.sep)&&relative!=='..'&&!path.isAbsolute(relative);}
if(!within(root,dir))throw Error('Evidence must stay in project');
if(fs.existsSync(dir))throw Error('Evidence directory already exists; refusing overwrite: '+dir);
let ancestor=path.dirname(dir);while(!fs.existsSync(ancestor))ancestor=path.dirname(ancestor);
const realRoot=fs.realpathSync(root),realAncestor=fs.realpathSync(ancestor);
if(realAncestor!==realRoot&&!within(realRoot,realAncestor))throw Error('Evidence parent resolves outside project');
fs.mkdirSync(path.dirname(dir),{recursive:true});
const realParent=fs.realpathSync(path.dirname(dir));
if(realParent!==realRoot&&!within(realRoot,realParent))throw Error('Evidence parent resolves outside project');
fs.mkdirSync(dir); // exclusive creation: a concurrent run must fail here
const startedUTC=new Date().toISOString(),database=path.join(dir,'synthetic.db');
const csv=path.join(dir,'samples.csv');
fs.writeFileSync(csv,'utc,elapsed_s,instance_id,successful_writes,completed_writes,completed_per_second,running,heap_alloc_bytes,goroutines,rss_bytes,db_bytes,wal_bytes,shm_bytes,sampled_target_id,sampled_due_wait_ms,sampled_queue_delay_ms,peak_upstream_active,same_url_overlaps\n');
let app,upstream,base,active=0,peak=0,overlap=0,logs='',storageFailure='',cancelled='';
let start=null,measurementElapsed=0,mutations=0,queries=0,restart=null,environment=null,progress={};
const health=new SoakHealth(),perURL=new Map(),seenKinds=new Set(),deletedIDs=new Set();
const segments=[];let finalTargets=[];
const abort=signal=>{cancelled=signal;};
const onINT=()=>abort('SIGINT'),onTERM=()=>abort('SIGTERM');
process.on('SIGINT',onINT);process.on('SIGTERM',onTERM);
function checkCancelled(){if(cancelled)throw Error('Experiment interrupted: '+cancelled);}
function atomic(name,value){const file=path.join(dir,name),tmp=file+'.'+process.pid+'.tmp';fs.writeFileSync(tmp,JSON.stringify(value,null,2)+'\n');fs.renameSync(tmp,file);}
function report(phase,extra={}){
 progress={...progress,phase,worker_pid:process.pid,app_pid:app?.child.pid??null,started_utc:startedUTC,updated_utc:new Date().toISOString(),configured_seconds:duration,elapsed_seconds:measurementElapsed,instance_id:health.instanceID,completed_writes:health.completed,...extra};atomic('progress.json',progress);
}
report('building');
function hashFiles(files){const hash=createHash('sha256');const entries=[];for(const file of files.sort()){const bytes=fs.readFileSync(path.join(root,file));entries.push({path:file,sha256:createHash('sha256').update(bytes).digest('hex')});hash.update(file+'\0');hash.update(bytes);}return {sha256:hash.digest('hex'),files:entries};}
function tree(relative){const result=[];for(const entry of fs.readdirSync(path.join(root,relative),{withFileTypes:true})){const file=path.posix.join(relative,entry.name);if(entry.isDirectory())result.push(...tree(file));else if(entry.isFile())result.push(file);}return result;}
function inspectLogs(chunk){const text=String(chunk);fs.appendFileSync(path.join(dir,'app.log'),text);logs=(logs+text).slice(-65536);if(/保存目标.*结果失败|读取.*失败|database.*(?:locked|error)|disk.*(?:full|error)/i.test(text)||/保存目标.*结果失败|读取.*失败|database.*(?:locked|error)|disk.*(?:full|error)/i.test(logs))storageFailure='Background storage error during soak';}
async function launch(exe,args){const launched=await launchServer(exe,args);app=launched;base=app.url;app.child.stderr.on('data',inspectLogs);return launched;}
async function api(route,method='GET',data){checkCancelled();const response=await fetch(base+route,{method,headers:data?{'Content-Type':'application/json'}:{},body:data?JSON.stringify(data):undefined,signal:AbortSignal.timeout(10000)});if(!response.ok)throw Error(method+' '+route+' HTTP'+response.status);return response.status===204?null:response.json();}
async function absent(id){for(const suffix of ['/checks','/daily?days=1']){const response=await fetch(base+'/api/targets/'+id+suffix,{signal:AbortSignal.timeout(10000)});if(response.status!==404)throw Error('Deleted target history revived: '+id+' '+suffix+' HTTP'+response.status);}}
function input(url,i){return {name:'synthetic-'+i,url,interval_seconds:10,timeout_seconds:1,enabled:true,...(i===0?{rules:{json_equals:[{pointer:'/code',equals:0}]}}:{})};}
function editable(t,changes){return {name:t.name,url:t.url,interval_seconds:t.interval_seconds,timeout_seconds:t.timeout_seconds,enabled:t.enabled,...(t.rules?{rules:t.rules}:{}),...changes};}
function size(file){try{return fs.statSync(file).size;}catch(error){if(error.code==='ENOENT')return 0;throw error;}}
function rss(pid){if(process.platform==='win32'){const result=spawnSync(process.env.PULSE_PWSH||'pwsh',['-NoProfile','-Command',`(Get-Process -Id ${pid} -ErrorAction Stop).WorkingSet64`],{encoding:'utf8',windowsHide:true});if(result.status!==0||!Number.isFinite(Number(result.stdout.trim())))throw Error('RSS sampler failed');return Number(result.stdout.trim());}if(process.platform==='linux'){const match=fs.readFileSync('/proc/'+pid+'/status','utf8').match(/^VmRSS:\s+(\d+)\s+kB/m);if(!match)throw Error('RSS sampler failed');return Number(match[1])*1024;}return '';}
function configSnapshot(t){return JSON.stringify(editable(t,{}));}
async function main(){
 const executable=path.join(dir,process.platform==='win32'?'pulse-soak.exe':'pulse-soak');
 const bundled=path.join(root,'.tools/go/bin/go.exe'),go=fs.existsSync(bundled)?bundled:'go';
 const env={...process.env,GOCACHE:path.join(root,'.cache/build'),GOTELEMETRY:'off'};if(fs.existsSync(bundled))env.GOPATH=path.join(root,'.cache/gopath');
 const buildArgs=['build','-trimpath','-ldflags=-s -w','-buildvcs=false','-o',executable,'.'];
 const product=hashFiles([...fs.readdirSync(root).filter(n=>n.endsWith('.go')&&!n.endsWith('_test.go')),'go.mod','go.sum',...tree('web')]);
 const runtime=hashFiles(['scripts/soak.cjs','scripts/soak-health.cjs','scripts/launch-server.cjs']);
 const commit=spawnSync('git',['-c','safe.directory='+root.split(path.sep).join('/'),'rev-parse','HEAD'],{cwd:root,encoding:'utf8',windowsHide:true});if(commit.status!==0)throw Error('Cannot record source commit');
 const goVersion=spawnSync(go,['version'],{cwd:root,encoding:'utf8',windowsHide:true});if(goVersion.status!==0)throw Error('Cannot record Go version');
 const build=spawnSync(go,buildArgs,{cwd:root,stdio:'inherit',env,windowsHide:true});if(build.status!==0)throw Error('Build failed');
 environment={source_commit:commit.stdout.trim(),product_source:product,runtime_scripts:runtime,binary_sha256:createHash('sha256').update(fs.readFileSync(executable)).digest('hex'),build:{command:go,args:buildArgs,go_version:goVersion.stdout.trim(),GOCACHE:env.GOCACHE,GOTELEMETRY:env.GOTELEMETRY,GOPATH:env.GOPATH??null},started_utc:startedUTC,duration_seconds:duration,mutation_seconds:mutationSeconds,platform:process.platform,arch:process.arch,node:process.version,targets:100,concurrency:4,interval_seconds:10,worker_pid:process.pid,database,notes:{write_count:'successful_writes minus first measurement baseline; one instance only',history:'stats.samples is retained history only, never progress',due_wait:'one rotating target sampled: now-next_check_at when enabled and not running',queue_delay:'same rotating target, running_since minus next_check_at if running; sampled, not full-population latency',memory:'RSS is working set; Go heap is heap_alloc_bytes; DB/WAL/SHM are allocated file sizes, not live record bytes',restart:'separate post-measurement forced-stop persistence check; no counter subtraction across instances'}};
 atomic('environment.json',environment);report('starting',{source_commit:environment.source_commit,product_sha256:product.sha256,runtime_sha256:runtime.sha256,binary_sha256:environment.binary_sha256});
 upstream=http.createServer((req,res)=>{
  const key=req.url;active++;peak=Math.max(peak,active);perURL.set(key,(perURL.get(key)||0)+1);if(perURL.get(key)>1)overlap++;
  let ended=false;const timer=setTimeout(()=>{if(key.startsWith('/slow/'))res.end('slow');else if(key.startsWith('/error/'))res.writeHead(500).end('error');else if(key.startsWith('/json/'))res.end('{"code":1}');else res.end('ok');},key.startsWith('/slow/')?2000:50);
  const end=()=>{if(ended)return;ended=true;clearTimeout(timer);active--;perURL.set(key,perURL.get(key)-1);};res.once('close',end);res.once('finish',end);
 });upstream.listen(0,'127.0.0.1');await once(upstream,'listening');
 const upstreamBase='http://127.0.0.1:'+upstream.address().port;
 const args=['serve','-addr','127.0.0.1:0','-concurrency','4','-db',database];
 environment.server_args=args;await launch(executable,args);environment.app_pid=app.child.pid;atomic('environment.json',environment);
 let victim;
 for(let i=0;i<100;i++){const route=i===0?'json':i===1?'error':i===2?'slow':'ok';const target=await api('/api/targets','POST',input(upstreamBase+'/'+route+'/'+i,i));if(i===99)victim=target;}
 let nextMutation=mutationSeconds,nextRSS=0,lastRSS='',previousElapsed=0,previousCompleted=0,sampleIndex=0,pendingResume=null,measurementStartWall=null;
 start=performance.now();report('measuring');
 while(true){
  checkCancelled();if(storageFailure)throw Error(storageFailure);
  const [status,targets]=await Promise.all([api('/api/status'),api('/api/targets')]);
  const now=performance.now(),wall=Date.now()/1000;
  // The first successful snapshot is the measurement baseline. Setup traffic
  // and pre-baseline writes do not inflate the measured throughput.
  if(measurementStartWall===null){start=now;measurementStartWall=wall;}
  const elapsed=(now-start)/1000;measurementElapsed=elapsed;
  health.observe(elapsed,targets,storageFailure||logs,status,wall);
  if(targets.length!==100||status.running>4||overlap)throw Error('Correctness violation: '+JSON.stringify({targets:targets.length,running:status.running,overlap,upstream_socket_peak:peak}));
  for(const t of targets){if(deletedIDs.has(t.id))throw Error('Deleted target revived: '+t.id);if(t.latest)seenKinds.add(t.latest.kind);}
  finalTargets=targets;
  if(elapsed>=nextRSS){lastRSS=rss(app.child.pid);nextRSS=elapsed+60;}
  const rate=(health.completed-previousCompleted)/Math.max(.001,elapsed-previousElapsed);previousElapsed=elapsed;previousCompleted=health.completed;
  const t=targets[sampleIndex++%targets.length],serverNow=Number(status.server_time)||Date.now();
  const dueWait=t.enabled&&!t.running?Math.max(0,serverNow-t.next_check_at):0;
  const queue=t.running&&t.running_since?Math.max(0,t.running_since-t.next_check_at):'';
  fs.appendFileSync(csv,[new Date().toISOString(),elapsed.toFixed(3),health.instanceID,health.lastWrites,health.completed,rate.toFixed(3),status.running,status.heap_alloc_bytes,status.goroutines,lastRSS,size(database),size(database+'-wal'),size(database+'-shm'),t.id,dueWait,queue,peak,overlap].join(',')+'\n');
  report('measuring',{write_baseline:health.baseline,successful_writes:health.lastWrites,running:status.running,mutations,query_rounds:queries,seen_kinds:[...seenKinds],same_url_overlaps:overlap});
  // Read history and day buckets throughout the experiment, including the last
  // sample. A failed API call always ends the run instead of retrying it away.
  await api('/api/targets/'+t.id+'/checks?limit=5');await api('/api/targets/'+t.id+'/daily?days=1');queries++;
  if(pendingResume&&(elapsed>=pendingResume.at||elapsed>=duration)){
   const paused=targets.find(t=>t.id===pendingResume.id);if(!paused)throw Error('Paused target disappeared');await api('/api/targets/'+paused.id,'PUT',editable(paused,{enabled:true}));pendingResume=null;
  }
  if(elapsed>=duration)break;
  if(elapsed>=nextMutation){
   const oldID=victim.id;await api('/api/targets/'+oldID,'DELETE');deletedIDs.add(oldID);await absent(oldID);
   mutations++;victim=await api('/api/targets','POST',input(upstreamBase+'/ok/replacement-'+mutations,99));
   const edit=targets.find(t=>t.id!==oldID&&t.id!==1&&t.id!==pendingResume?.id);
   await api('/api/targets/'+edit.id,'PUT',editable(edit,{name:'edited-'+mutations}));
   const paused=targets.find(t=>t.id!==oldID&&t.id!==edit.id&&t.id>3&&t.enabled&&t.id!==pendingResume?.id);
   if(!pendingResume&&paused){await api('/api/targets/'+paused.id,'PUT',editable(paused,{enabled:false}));pendingResume={id:paused.id,at:elapsed+10};}
   nextMutation=elapsed+mutationSeconds;
  }
  // Schedule against the completed snapshot, so API/RSS costs do not silently
  // extend the nominal ten-second sampling interval.
  await delay(Math.max(10,Math.min(now+10000,start+duration*1000)-performance.now()));
 }
 if(duration>=60&&!['ok','http_error','timeout','assertion_failed'].every(k=>seenKinds.has(k)))throw Error('Missing mixed result kind');
 for(const id of deletedIDs)await absent(id);
 segments.push({instance_id:health.instanceID,started_utc:new Date(measurementStartWall*1000).toISOString(),elapsed_seconds:measurementElapsed,write_baseline:health.baseline,write_final:health.lastWrites,completed_writes:health.completed});
 // Refresh configs after any final resume, and compare persisted configurations
 // and latest IDs; a target-count-only check would miss lost configurations.
 finalTargets=await api('/api/targets');
 const expected=new Map(finalTargets.map(t=>[t.id,{config:configSnapshot(t),latest:t.latest?.id??0}]));
 const restartStart=performance.now();report('restart-check');
 const oldInstance=health.instanceID;restart={passed:false,phase:'running',started_utc:new Date().toISOString(),previous_instance_id:oldInstance};atomic('restart.json',restart);
await stopChild(app.child);app=null;
 await launch(executable,args);
 const [restored,restartedStatus]=await Promise.all([api('/api/targets'),api('/api/status')]);
 if(restored.length!==100||restartedStatus.monitor_health?.state==='degraded'||!restartedStatus.monitor_health?.instance_id||restartedStatus.monitor_health.instance_id===oldInstance)throw Error('Persistence restart health/instance failed');
 for(const t of restored){const saved=expected.get(t.id);if(!saved||saved.config!==configSnapshot(t)||(t.latest?.id??0)<saved.latest)throw Error('Persistence after process restart failed for target '+t.id);}
 for(const id of deletedIDs)await absent(id);
 restart={...restart,passed:true,phase:'completed',ended_utc:new Date().toISOString(),elapsed_seconds:(performance.now()-restartStart)/1000,previous_instance_id:oldInstance,instance_id:restartedStatus.monitor_health.instance_id,successful_writes_at_observation:restartedStatus.monitor_health.successful_writes,targets:restored.length,termination:'forced isolated-child stop; graceful SIGINT/Ctrl+C verified separately; restart duration excluded from measurement'};
 atomic('restart.json',restart);checkCancelled();if(storageFailure)throw Error(storageFailure);
 return {configured_seconds:duration,elapsed_seconds:measurementElapsed,completed_writes:health.completed,segments,peak_upstream_active:peak,same_url_overlaps:overlap,mutations,query_rounds:queries,deleted_targets:deletedIDs.size,seen_kinds:[...seenKinds],restart};
}
async function run(){
 let result,error;
 try{result=await main();}catch(cause){error=cause;if(restart&&restart.phase==='running'){restart={...restart,phase:'failed',ended_utc:new Date().toISOString(),error:String(cause.stack||cause)};try{atomic('restart.json',restart);}catch(recordError){console.error(recordError);}}}
 try{report('cleaning');}catch(cause){error=error||cause;}
 const cleanupErrors=[];
 try{await stopChild(app?.child);app=null;}catch(cause){cleanupErrors.push(String(cause));error=error||cause;}
 try{if(upstream){upstream.closeAllConnections();await new Promise(resolve=>upstream.close(resolve));}}catch(cause){cleanupErrors.push(String(cause));error=error||cause;}
 if(cancelled&&!error)error=Error('Experiment interrupted: '+cancelled);
 process.removeListener('SIGINT',onINT);process.removeListener('SIGTERM',onTERM);
 const terminal={...result,passed:!error,phase:error?'failed':'completed',started_utc:startedUTC,ended_utc:new Date().toISOString(),worker_pid:process.pid,configured_seconds:duration,elapsed_seconds:measurementElapsed,completed_writes:health.completed,instance_id:health.instanceID,segments,restart,cleanup:cleanupErrors.length?{passed:false,errors:cleanupErrors}:{passed:true,detail:'child and upstream stopped'},...(error?{error:String(error.stack||error)}:{})};
 if(error){fs.writeFileSync(path.join(dir,'failure.txt'),String(error.stack||error)+'\n');process.exitCode=1;console.error(error);}
 try{report(error?'failed':'completed',{passed:!error,error:error?String(error.message):null,app_pid:null,ended_utc:terminal.ended_utc});}catch(cause){terminal.passed=false;terminal.phase='failed';terminal.error=String(cause.stack||cause);process.exitCode=1;console.error(cause);}
 try{atomic('result.json',terminal);}catch(cause){terminal.passed=false;terminal.phase='failed';terminal.error=String(cause.stack||cause);process.exitCode=1;console.error(cause);}
 console.log(JSON.stringify(terminal));
}
run();
