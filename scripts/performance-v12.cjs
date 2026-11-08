// Opt-in, sequential binary experiment. Never opens user data or reuses samples.
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const http = require('node:http');
const { once } = require('node:events');
const { spawnSync, execFile } = require('node:child_process');
const { promisify } = require('node:util');
const { createHash } = require('node:crypto');
const { performance } = require('node:perf_hooks');
const { setTimeout: delay } = require('node:timers/promises');
const { DatabaseSync } = require('node:sqlite');
const { launchServer, stopChild } = require('./launch-server.cjs');
const execFileAsync = promisify(execFile);
const root = path.resolve(__dirname, '..');
const ownedChildren=new Set();let interrupted;
function checkInterrupted(){if(interrupted)throw interrupted;}
async function launchOwned(binary,args){checkInterrupted();const app=await launchServer(binary,args);ownedChildren.add(app.child);app.child.once('exit',()=>ownedChildren.delete(app.child));checkInterrupted();return app;}
const sha = data => createHash('sha256').update(data).digest('hex');
const columns = ['version','targets','concurrency','round','wall_ms','actual_writes','writes_per_second','peak_upstream_active','peak_heap_bytes','peak_goroutines','rss_before_bytes','rss_after_bytes','status_samples'];
function median(values) {
  if (!values.length || values.some(n => !Number.isFinite(n))) throw Error('Invalid median input');
  const sorted = [...values].sort((a,b) => a-b), middle = sorted.length >> 1;
  return sorted.length % 2 ? sorted[middle] : (sorted[middle-1] + sorted[middle])/2;
}
function matrixCases(preflight = false) {
  return (preflight ? [2] : [10,100]).flatMap(targets => (preflight ? [1] : [1,4,16]).flatMap(concurrency => Array.from({length:preflight?1:3},(_,n) => ({targets,concurrency,round:n+1}))));
}
function validateWrites(rows, targets) {
  if (rows.length !== targets || rows.some(r => r.n !== 1 || r.failures !== 0) || new Set(rows.map(r => r.target_id)).size !== targets) throw Error('Missing, duplicate or failed checks: '+JSON.stringify(rows));
}
function comparisons(rows, keys, metric) {
  const grouped = new Map();
  for (const row of rows) {
    const key = keys.map(k => row[k]).join('/');
    if (!grouped.has(key)) grouped.set(key, {});
    const group = grouped.get(key); (group[row.version] ||= []).push(Number(row[metric]));
  }
  return [...grouped].filter(([,g]) => g.baseline && g.candidate).map(([key,g]) => {
    const baseline_ms = median(g.baseline), candidate_ms = median(g.candidate), delta_ms = candidate_ms-baseline_ms;
    return {metric,key,baseline_ms,candidate_ms,delta_ms,change_percent:100*delta_ms/baseline_ms,investigation_required:delta_ms>.1 && candidate_ms>baseline_ms*1.2};
  });
}
function rawCSV(rows, fields) {
  const quote=value=>'"'+String(value??'').replaceAll('"','""')+'"';
  return fields.map(quote).join(',')+'\n'+rows.map(row=>fields.map(field=>quote(row[field])).join(',')).join('\n')+'\n';
}
function safeOutput(destination) {
  const out = path.resolve(destination), relative = path.relative(path.join(root,'.cache'),out);
  if (!relative || relative.startsWith('..') || path.isAbsolute(relative)) throw Error('Output must be a NEW child directory under project .cache');
  // Reject junction/symlink ancestors so a lexical project path cannot escape.
  const parent = path.dirname(out);
  const boundary = path.dirname(root);
  function checkAncestors() {
    let cursor = parent;
    // path.relative follows the platform's case rules (including Windows drive
    // letters). The fixed-point guard bounds traversal at a filesystem root.
    while (path.relative(boundary,cursor) !== '') {
      try { if (fs.lstatSync(cursor).isSymbolicLink()) throw Error('Symlink output ancestor rejected'); }
      catch(error) { if(error.code!=='ENOENT')throw error; }
      const next = path.dirname(cursor);
      if (next === cursor) break;
      cursor = next;
    }
  }
  checkAncestors();
  const expectedParent = path.resolve(fs.realpathSync(root),path.relative(root,parent));
  fs.mkdirSync(parent, {recursive:true});
  // Recheck after creation: missing parents are allowed, redirected parents are
  // not. The final evidence directory itself must still be newly created.
  checkAncestors();
  if(path.relative(expectedParent,fs.realpathSync(parent))!=='')throw Error('Output parent resolved outside expected project path');
  fs.mkdirSync(out, {recursive:false});
  return out;
}
function sourceFingerprint(directory) {
  const files=[];
  function walk(base) { for(const entry of fs.readdirSync(path.join(directory,base),{withFileTypes:true})) { const name=base?base+'/'+entry.name:entry.name; if(entry.isDirectory()) walk(name); else files.push(name); } }
  for(const file of fs.readdirSync(directory).filter(n => n.endsWith('.go')&&!n.endsWith('_test.go') || ['go.mod','go.sum'].includes(n))) files.push(file);
  walk('web'); files.sort();
  const digest=createHash('sha256'); for(const file of files) {digest.update(file+'\0');digest.update(fs.readFileSync(path.join(directory,file)));}
  return {sha256:digest.digest('hex'),files};
}
async function rss(pid) {
  if(process.platform==='linux') {const match=fs.readFileSync('/proc/'+pid+'/status','utf8').match(/^VmRSS:\s+(\d+) kB/m);return match?Number(match[1])*1024:null;}
  if(process.platform==='win32') {const result=await execFileAsync(process.env.PULSE_PWSH||'pwsh',['-NoProfile','-Command',`(Get-Process -Id ${pid} -ErrorAction Stop).WorkingSet64`],{windowsHide:true,timeout:10000});const n=Number(result.stdout.trim());if(!Number.isFinite(n)||n<=0)throw Error('Invalid RSS');return n;}
  return null;
}
async function api(app, route, method='GET', data) {
  checkInterrupted();
  const response=await fetch(app.url+route,{method,signal:AbortSignal.timeout(10000),headers:data?{'Content-Type':'application/json'}:{},body:data?JSON.stringify(data):undefined});
  if(!response.ok)throw Error(method+' '+route+' HTTP '+response.status);
  return response.status===204?null:response.json();
}
function input(url, i) {return {name:'synthetic-'+i,url,enabled:false,interval_seconds:86400,timeout_seconds:5};}
async function seedTargets(binary, file, count, upstream) {
  let app;
  try {app=await launchOwned(binary,['serve','-addr','127.0.0.1:0','-db',file]);for(let i=0;i<count;i++)await api(app,'/api/targets','POST',input(upstream+'/check/'+i,i));}
  finally {await stopChild(app?.child);}
}
async function matrixRound(binary, version, fixtureDir, testCase, upstream, activity, rawSample) {
  const {targets,concurrency,round}=testCase, file=path.join(fixtureDir,`${version}-matrix-${targets}-${concurrency}-${round}.db`);
  await seedTargets(binary,file,targets,upstream);
  let app,db;
  try {
    app=await launchOwned(binary,['serve','-addr','127.0.0.1:0','-db',file,'-concurrency',String(concurrency)]);
    db=new DatabaseSync(file); db.exec('PRAGMA busy_timeout=5000');
    if(db.prepare('PRAGMA user_version').get().user_version!==(version==='baseline'?3:4))throw Error('Supplied binary has wrong schema for '+version);
    const before=await rss(app.child.pid);activity.reset();
    let heap=0,goroutines=0,samples=0,nextStatus=0;
    const started=performance.now();
    // All targets become due in ONE transaction. No creation-time stagger and no old samples.
    db.exec('BEGIN; UPDATE targets SET enabled=1,next_check_at=0; COMMIT;');
    let completed=0;
    while(true) {
      checkInterrupted();
      const elapsed=performance.now()-started;
      completed=db.prepare('SELECT COUNT(*) AS n FROM checks').get().n;
      if(completed>=targets)break;
      if(elapsed>90000)throw Error(`Round timeout: ${completed}/${targets}`);
      if(elapsed>=nextStatus) {
        const s=await api(app,'/api/status');heap=Math.max(heap,s.heap_alloc_bytes);goroutines=Math.max(goroutines,s.goroutines);samples++;
        rawSample({version,...testCase,elapsed_ms:performance.now()-started,actual_writes:completed,heap_alloc_bytes:s.heap_alloc_bytes,goroutines:s.goroutines,running:s.running});
        if(s.running>concurrency)throw Error('Scheduler concurrency exceeded');
        nextStatus=elapsed+50;
      }
      await delay(5);
    }
    const wall=performance.now()-started;
    // Let outstanding finish/close notifications settle, then inspect exact persisted identities.
    await delay(75);
    validateWrites(db.prepare("SELECT target_id,COUNT(*) AS n,SUM(kind!='ok') AS failures FROM checks GROUP BY target_id ORDER BY target_id").all(),targets);
    if(activity.overlap || activity.peak>concurrency || activity.requests!==targets || activity.active!==0)throw Error('Upstream invariant failed: '+JSON.stringify(activity.snapshot()));
    const finalStatus=await api(app,'/api/status');heap=Math.max(heap,finalStatus.heap_alloc_bytes);goroutines=Math.max(goroutines,finalStatus.goroutines);
    return {version,...testCase,wall_ms:wall,actual_writes:completed,writes_per_second:completed*1000/wall,peak_upstream_active:activity.peak,peak_heap_bytes:heap,peak_goroutines:goroutines,rss_before_bytes:before,rss_after_bytes:await rss(app.child.pid),status_samples:samples+1};
  } finally {db?.close();await stopChild(app?.child);}
}
function populateStorage(file, count, each) {
  const db=new DatabaseSync(file);let committed=false;
  try {
    const hasCategory=db.prepare('PRAGMA table_info(checks)').all().some(c=>c.name==='error_category');
    const statement=db.prepare('INSERT INTO checks(target_id,input_url,final_url,status_code,duration_ms,kind,detail,checked_at,total_duration_ms,config_version,rules_snapshot'+(hasCategory?',error_category':'')+') VALUES(?,?,?,?,?,?,?,?,50,1,\'{}\''+(hasCategory?',?':'')+')');
    const buckets=new Map(),today=new Date().toISOString().slice(0,10),origin=Date.parse(today+'T00:00:00Z');
    const kinds=[['ok',200,''],['http_error',503,'http_5xx'],['timeout',0,'timeout'],['assertion_failed',200,'assertion'],['request_error',0,'dns']];
    db.exec('BEGIN');
    for(let id=1;id<=count;id++)for(let n=0;n<each;n++) {
      const day=n%30, at=origin-day*86400000+Math.min(1,Math.max(0,Date.now()-origin-1)),date=new Date(at).toISOString().slice(0,10);
      const [kind,status,category]=kinds[n%kinds.length],duration=kind==='timeout'?1000:50;
      const args=[id,'http://127.0.0.1/synthetic','http://127.0.0.1/synthetic',status,duration,kind,'synthetic mixed fixture',at];if(hasCategory)args.push(category);statement.run(...args);
      const key=id+'/'+date, bucket=buckets.get(key)||{id,date,samples:0,successes:0,sum:0,max:0};bucket.samples++;bucket.successes+=Number(kind==='ok');bucket.sum+=duration;bucket.max=Math.max(bucket.max,duration);buckets.set(key,bucket);
    }
    const daily=db.prepare('INSERT INTO daily_summaries(target_id,date,samples,successes,duration_sum_ms,max_ms,migration_partial) VALUES(?,?,?,?,?,?,0)');
    for(const b of buckets.values())daily.run(b.id,b.date,b.samples,b.successes,b.sum,b.max);
    db.exec('COMMIT');committed=true;
    return {targets:count,history_each:each,total_checks:count*each,utc_days:Math.min(30,each),distribution:'n%5: ok,http_error503,timeout,assertion_failed,request_error(dns in schema4; legacy inferred unknown)',schema:db.prepare('PRAGMA user_version').get().user_version};
  } finally {if(!committed)try{db.exec('ROLLBACK');}catch{}db.close();}
}
async function storageRound(binary,version,folder,round,upstream,preflight) {
  const file=path.join(folder,`${version}-storage-${round}.db`),count=preflight?2:100,each=preflight?30:1000;
  await seedTargets(binary,file,count,upstream);const fixture=populateStorage(file,count,each);let app;
  if(fixture.schema!==(version==='baseline'?3:4))throw Error('Storage fixture has wrong version');
  const rows=[];
  try {
    app=await launchOwned(binary,['serve','-addr','127.0.0.1:0','-db',file]);
    const tests=[['list','/api/targets',r=>r.length===count],['history_first','/api/targets/1/checks?limit=20',r=>r.items.length===20],['history_page','/api/targets/1/checks?limit=10&before_id=21',r=>r.items.length===10&&r.items.every(i=>i.id<21)],['daily','/api/targets/1/daily?days=30',r=>r.buckets.length===30],['status','/api/status',r=>r.status==='ok']];
    for(const [operation,route,validate] of tests) {
      await api(app,route); // One explicit warmup, excluded from raw observations.
      for(let sample=1;sample<=(preflight?2:10);sample++) {const start=performance.now(),response=await api(app,route),elapsed_ms=performance.now()-start;if(!validate(response))throw Error('Storage response invalid: '+operation);rows.push({version,round,operation,sample,elapsed_ms});}
    }
    const history=await api(app,'/api/targets/1/checks?limit=1');
    rows.push({version,round,operation:'categories_http',sample:0,elapsed_ms:'N/A',note:history.error_categories?'included in history endpoint; isolated Store cost in Go benchmark':'N/A: v1.1 has no category summary'});
    rows.push({version,round,operation:'monitor_health',sample:0,elapsed_ms:'N/A',note:(await api(app,'/api/status')).monitor_health?'included in status endpoint':'N/A: v1.1 has no monitor health'});
    // API manual-check combines a real 50ms request, scheduling and write. The isolated
    // RecordCheck cost and allocations are measured separately in the Go benchmark.
    const db=new DatabaseSync(file,{readOnly:true});
    try {for(let sample=1;sample<=(preflight?1:3);sample++) {const before=db.prepare('SELECT MAX(id) AS id FROM checks').get().id,start=performance.now();await api(app,'/api/targets/1/check','POST');while(db.prepare('SELECT MAX(id) AS id FROM checks').get().id<=before){checkInterrupted();if(performance.now()-start>10000)throw Error('Write completion timeout');await delay(2);}rows.push({version,round,operation:'check_and_write_50ms',sample,elapsed_ms:performance.now()-start});}}
    finally {db.close();}
    return {fixture,rows};
  } finally {await stopChild(app?.child);}
}
function runCommand(exe,args,options={}) {
  const result=spawnSync(exe,args,{cwd:root,windowsHide:true,encoding:'utf8',timeout:180000,...options});
  if(result.error||result.status!==0)throw Error(`${exe} ${args.join(' ')} failed: ${result.error||result.stderr||result.stdout}`);
  return result.stdout;
}
function snapshotSources(folder,baselineRef) {
  const old=path.join(folder,'baseline-source'),current=path.join(folder,'candidate-source');fs.mkdirSync(old);fs.mkdirSync(current);
  const archive=path.join(folder,'baseline-source.tar');runCommand('git',['archive','--format=tar','--output='+archive,baselineRef]);runCommand('tar',['-xf',archive,'-C',old]);
  for(const name of fs.readdirSync(root).filter(n=>n.endsWith('.go')&&!n.endsWith('_test.go') || ['go.mod','go.sum'].includes(n)))fs.copyFileSync(path.join(root,name),path.join(current,name));
  fs.cpSync(path.join(root,'web'),path.join(current,'web'),{recursive:true});
  for(const dir of [old,current]) {
    for(const name of fs.readdirSync(dir).filter(n=>n.endsWith('_test.go')))fs.unlinkSync(path.join(dir,name));
    fs.copyFileSync(path.join(root,'performance_v12_test.go'),path.join(dir,'performance_v12_test.go'));
  }
  return {baseline:old,candidate:current};
}
function goBenchmarks(directory,version,out,preflight) {
  const bundled=path.join(root,'.tools','go','bin','go.exe'),go=process.env.PULSE_GO||(fs.existsSync(bundled)?bundled:'go');
  const env={...process.env,GOCACHE:path.join(root,'.cache','build'),GOPATH:path.join(root,'.cache','gopath'),GOTELEMETRY:'off',GOTOOLCHAIN:'local'};
  const args=['test','-run=^TestM4QueryPlans$','-bench=^BenchmarkM4','-benchtime='+(preflight?'1x':'10x'),'-count='+(preflight?'1':'3'),'-benchmem','-timeout=180s','-v'];
  const result=spawnSync(go,args,{cwd:directory,env,windowsHide:true,encoding:'utf8',timeout:210000});
  fs.writeFileSync(path.join(out,version+'-go-benchmark.txt'),(result.stdout||'')+(result.stderr||''));
  if(result.error||result.status!==0)throw Error(version+' Go benchmark failed: '+(result.error||result.stderr||result.stdout));
  return parseGoMeasurements(result.stdout,version,preflight);
}
function parseGoMeasurements(output,version,preflight) {
  const rows=[],counts=new Map();for(const line of output.split(/\r?\n/)) {const m=line.match(/^(BenchmarkM4\S+)-\d+\s+\d+\s+([\d.]+)\s+ns\/op\s+([\d.]+)\s+B\/op\s+([\d.]+)\s+allocs\/op/);if(m){const round=(counts.get(m[1])||0)+1;counts.set(m[1],round);rows.push({version,operation:m[1],round,elapsed_ms:Number(m[2])/1e6,bytes_per_op:Number(m[3]),allocs_per_op:Number(m[4])});}}
  if(rows.length!==(version==='baseline'?9:10)*(preflight?1:3)||[...counts.values()].some(n=>n!==(preflight?1:3)))throw Error('Missing, repeated or unparsed Go measurements');return rows;
}
async function main(argv=process.argv.slice(2)) {
  if(argv.length<2)throw Error('Usage: node scripts/performance-v12.cjs <v1.1 binary> <candidate binary> [--preflight] [--skip-go] [--out=project/.cache/new-directory] [--baseline-ref=fbfd0be]');
  const binaries={baseline:path.resolve(argv[0]),candidate:path.resolve(argv[1])};for(const binary of Object.values(binaries))if(!fs.statSync(binary).isFile())throw Error('Not a binary file');
  const options=argv.slice(2),preflight=options.includes('--preflight'),skipGo=options.includes('--skip-go');
  for(const option of options)if(!['--preflight','--skip-go'].includes(option)&&!option.startsWith('--out=')&&!option.startsWith('--baseline-ref='))throw Error('Unknown option '+option);
  const out=safeOutput(options.find(a=>a.startsWith('--out='))?.slice(6)||path.join(root,'.cache','performance-v12-'+Date.now()));
  const fixtureDir=fs.mkdtempSync(path.join(os.tmpdir(),'pulse-m4-performance-'));let upstream;
  const onInterrupt=signal=>{interrupted=Error('Experiment interrupted by '+signal);for(const child of ownedChildren)child.kill('SIGKILL');};
  const onSIGINT=()=>onInterrupt('SIGINT'),onSIGTERM=()=>onInterrupt('SIGTERM');process.once('SIGINT',onSIGINT);process.once('SIGTERM',onSIGTERM);
  const started=performance.now(),matrix=[],storage=[],bench=[],samples=[];let finalResult;
  const writeJSON=(name,data)=>fs.writeFileSync(path.join(out,name),JSON.stringify(data,null,2)+'\n');
  const activity={active:0,peak:0,overlap:0,requests:0,urls:new Map(),reset(){if(this.active)throw Error('Previous upstream still active');this.peak=this.overlap=this.requests=0;this.urls.clear();},snapshot(){return {active:this.active,peak:this.peak,overlap:this.overlap,requests:this.requests};}};
  const flush=()=>{fs.writeFileSync(path.join(out,'matrix.csv'),rawCSV(matrix,columns));fs.writeFileSync(path.join(out,'storage-raw.csv'),rawCSV(storage,['version','round','operation','sample','elapsed_ms','note']));fs.writeFileSync(path.join(out,'go-raw.csv'),rawCSV(bench,['version','round','operation','elapsed_ms','bytes_per_op','allocs_per_op']));fs.writeFileSync(path.join(out,'matrix-status-samples.csv'),rawCSV(samples,['version','targets','concurrency','round','elapsed_ms','actual_writes','heap_alloc_bytes','goroutines','running']));writeJSON('storage-raw.json',storage);writeJSON('go-raw.json',bench);writeJSON('matrix-status-samples.json',samples);};
  const environment={started_utc:new Date().toISOString(),preflight,platform:process.platform,arch:process.arch,node:process.version,cpu:os.cpus()[0]?.model,logical_cpus:os.cpus().length,total_memory_bytes:os.totalmem(),upstream_delay_ms:50,status_sample_interval_ms:50,write_poll_interval_ms:5,matrix_timing:'single activation transaction through final observed SQLite commit; excludes process startup; includes scheduler scan wait and sampling overhead',rss:'before activation and after completion only; not peak RSS',source_current:sourceFingerprint(root),runner_sha256:sha(fs.readFileSync(__filename)),benchmark_sha256:sha(fs.readFileSync(path.join(root,'performance_v12_test.go'))),binaries:{},baseline_ref:options.find(a=>a.startsWith('--baseline-ref='))?.slice(15)||'fbfd0be',go_benchmarks:skipGo?'explicitly skipped; no allocations evidence':'source snapshots (separate executables from supplied binary matrix)'};
  try {
    for(const [version,binary] of Object.entries(binaries))environment.binaries[version]={path:binary,sha256:sha(fs.readFileSync(binary))};
    environment.git_head=runCommand('git',['rev-parse','HEAD']).trim();environment.git_status=runCommand('git',['status','--porcelain']);
    environment.baseline_commit=runCommand('git',['rev-parse','--verify','--end-of-options',environment.baseline_ref+'^{commit}']).trim();
    const bundled=path.join(root,'.tools','go','bin','go.exe'),go=process.env.PULSE_GO||(fs.existsSync(bundled)?bundled:'go');
    environment.go=runCommand(go,['version']).trim();
    for(const version of Object.keys(binaries))environment.binaries[version].go_build_info=runCommand(go,['version','-m',binaries[version]]).trim();
    environment.binary_source_correspondence='SHA256/build metadata identify supplied binaries; source benchmark hashes identify separately compiled snapshots. Verify provenance before release; a source hash alone does not prove which bytes built an externally supplied binary.';
    const sources=skipGo?null:snapshotSources(fixtureDir,environment.baseline_commit);
    if(sources) {environment.source_baseline=sourceFingerprint(sources.baseline);environment.source_candidate_snapshot=sourceFingerprint(sources.candidate);}
    writeJSON('environment.json',environment);
    upstream=http.createServer((req,res)=> {const key=req.url;activity.active++;activity.requests++;activity.peak=Math.max(activity.peak,activity.active);activity.urls.set(key,(activity.urls.get(key)||0)+1);if(activity.urls.get(key)>1)activity.overlap++;const timer=setTimeout(()=>res.end('synthetic fixed 50ms'),50);let done=false;const finish=()=>{if(done)return;done=true;clearTimeout(timer);activity.active--;activity.urls.set(key,activity.urls.get(key)-1);};res.once('finish',finish);res.once('close',finish);});
    upstream.listen(0,'127.0.0.1');await once(upstream,'listening');const base='http://127.0.0.1:'+upstream.address().port;
    for(const [version,binary] of Object.entries(binaries)) {
      // Versions and all rounds are strictly serial. Every fixture is new.
      for(const testCase of matrixCases(preflight)) {writeJSON('progress.json',{stage:'matrix',version,...testCase});matrix.push(await matrixRound(binary,version,fixtureDir,testCase,base,activity,row=>samples.push(row)));flush();}
      for(let round=1;round<=(preflight?1:3);round++){writeJSON('progress.json',{stage:'storage',version,round});const result=await storageRound(binary,version,fixtureDir,round,base,preflight);writeJSON(version+'-fixture-'+round+'.json',result.fixture);storage.push(...result.rows);flush();}
      if(sources) {bench.push(...goBenchmarks(sources[version],version,out,preflight));flush();}
    }
    if(sourceFingerprint(root).sha256!==environment.source_current.sha256 || sha(fs.readFileSync(__filename))!==environment.runner_sha256 || sha(fs.readFileSync(path.join(root,'performance_v12_test.go')))!==environment.benchmark_sha256)throw Error('Runtime source or experiment harness changed during measurement');
    for(const version of Object.keys(binaries))if(sha(fs.readFileSync(binaries[version]))!==environment.binaries[version].sha256)throw Error('Supplied binary changed during measurement');
    const compare=[...comparisons(matrix,['targets','concurrency'],'wall_ms'),...comparisons(storage.filter(r=>typeof r.elapsed_ms==='number'),['operation'],'elapsed_ms'),...comparisons(bench,['operation'],'elapsed_ms')];
    writeJSON('comparisons.json',compare);
    const investigations=compare.filter(r=>r.investigation_required);writeJSON('investigations-required.json',investigations);
    const result={status:preflight?'preflight_completed':investigations.length?'investigation_required':'measurements_completed_review_required',correctness_passed:true,release_acceptance_passed:false,reason:preflight?'small controlled smoke test only':skipGo?'allocations/source benchmarks explicitly skipped':'human review of workload impact and source/binary correspondence still required; explanations alone do not waive material regressions',elapsed_seconds:(performance.now()-started)/1000,matrix_rows:matrix.length,expected_matrix_rows:preflight?2:36,storage_observations:storage.length,go_observations:bench.length,investigation_count:investigations.length,output:out};
    finalResult=result;
  } catch(error) {flush();writeJSON('result.json',{status:'failed',correctness_passed:false,release_acceptance_passed:false,error:String(error.stack),elapsed_seconds:(performance.now()-started)/1000});throw error;}
  finally {try {for(const child of ownedChildren)await stopChild(child);if(upstream){upstream.closeAllConnections();await new Promise(resolve=>upstream.close(resolve));}const resolved=fs.realpathSync(fixtureDir),temporary=fs.realpathSync(os.tmpdir());if(path.dirname(resolved)!==temporary||!path.basename(resolved).startsWith('pulse-m4-performance-'))throw Error('Refusing unsafe fixture cleanup');fs.rmSync(resolved,{recursive:true,force:true});} catch(error){writeJSON('result.json',{status:'failed',correctness_passed:false,release_acceptance_passed:false,cleanup_error:String(error.stack)});throw error;} finally{process.removeListener('SIGINT',onSIGINT);process.removeListener('SIGTERM',onSIGTERM);}}
  if(interrupted){writeJSON('result.json',{status:'failed',correctness_passed:false,release_acceptance_passed:false,error:String(interrupted)});throw interrupted;}
  finalResult.elapsed_seconds=(performance.now()-started)/1000;writeJSON('result.json',finalResult);console.log(JSON.stringify(finalResult));
}
module.exports={median,matrixCases,validateWrites,comparisons,safeOutput,populateStorage,sourceFingerprint,rawCSV,snapshotSources,parseGoMeasurements};
if(require.main===module)main().catch(error=>{console.error(error);process.exitCode=1;});
