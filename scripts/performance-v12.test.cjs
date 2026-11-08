const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const os=require('node:os');
const path=require('node:path');
const {spawnSync}=require('node:child_process');
const {DatabaseSync}=require('node:sqlite');
const {median,matrixCases,validateWrites,comparisons,safeOutput,populateStorage,rawCSV,parseGoMeasurements}=require('./performance-v12.cjs');

test('matrix enumerates exactly 18 unique rounds per version without old samples',()=>{
  const cases=matrixCases();assert.equal(cases.length,18);assert.equal(new Set(cases.map(c=>JSON.stringify(c))).size,18);
  for(const targets of [10,100])for(const concurrency of [1,4,16])assert.deepEqual(cases.filter(c=>c.targets===targets&&c.concurrency===concurrency).map(c=>c.round),[1,2,3]);
  assert.deepEqual(matrixCases(true),[{targets:2,concurrency:1,round:1}]);
});
test('medians do not mutate input and compare versions independently',()=>{
  const values=[100,1,2];assert.equal(median(values),2);assert.deepEqual(values,[100,1,2]);assert.equal(median([3,1]),2);assert.throws(()=>median([]));assert.throws(()=>median([NaN]));
  const rows=[1,2,100].map(elapsed_ms=>({version:'baseline',operation:'list',elapsed_ms})).concat([3,4,200].map(elapsed_ms=>({version:'candidate',operation:'list',elapsed_ms})));
  const [result]=comparisons(rows,['operation'],'elapsed_ms');assert.equal(result.baseline_ms,2);assert.equal(result.candidate_ms,4);assert.equal(result.investigation_required,true);
  assert.equal(comparisons([{version:'baseline',operation:'x',elapsed_ms:.01},{version:'candidate',operation:'x',elapsed_ms:.02}],['operation'],'elapsed_ms')[0].investigation_required,false);
});
test('Go parser preserves each independent round and rejects lost/duplicate observations',()=>{
  const lines=Array.from({length:3},()=>Array.from({length:9},(_,n)=>`BenchmarkM4/test${n}-20 10 2000000 ns/op 100 B/op 5 allocs/op`)).flat();
  const rows=parseGoMeasurements(lines.join('\n'),'baseline',false);assert.equal(rows.length,27);assert.deepEqual(rows.filter(r=>r.operation==='BenchmarkM4/test0').map(r=>r.round),[1,2,3]);assert.equal(rows[0].elapsed_ms,2);
  assert.throws(()=>parseGoMeasurements(lines.slice(1).join('\n'),'baseline',false));assert.throws(()=>parseGoMeasurements([...lines,lines[0]].join('\n'),'baseline',false));
  assert.equal(rawCSV([{value:'a,"b"\nc'}],['value']),'"value"\n"a,""b""\nc"\n');
});
test('exact persisted target counts reject omissions, duplicates and failures',()=>{
  const rows=[{target_id:1,n:1,failures:0},{target_id:2,n:1,failures:0}];validateWrites(rows,2);
  assert.throws(()=>validateWrites(rows.slice(1),2));assert.throws(()=>validateWrites([{target_id:1,n:2,failures:0},rows[1]],2));assert.throws(()=>validateWrites([rows[0],{target_id:2,n:1,failures:1}],2));assert.throws(()=>validateWrites([rows[0],rows[0]],2));
});
test('output cannot reuse or escape an existing output directory',()=>{
  assert.throws(()=>safeOutput(os.tmpdir()));assert.throws(()=>safeOutput(path.resolve(__dirname,'..','.cache')));assert.throws(()=>safeOutput(path.resolve(__dirname,'..','.cache','..','outside')));
  const output=path.resolve(__dirname,'..','.cache','performance-unit-'+process.pid+'-'+Date.now());
  safeOutput(output);try{assert.throws(()=>safeOutput(output));}finally{fs.rmdirSync(output);}
});
test('Windows output ancestor traversal terminates with different drive/path casing',{skip:process.platform!=='win32'},()=>{
  const output=path.resolve(__dirname,'..','.cache','performance-case-unit-'+process.pid+'-'+Date.now());
  // Isolate the traversal so this regression cannot hang the test worker.
  const differentlyCased=output[0].toLowerCase()+output.slice(1).replace('go-monitor','GO-MONITOR');
  const script='const {safeOutput}=require(process.argv[1]); safeOutput(process.argv[2]);';
  try {
    const result=spawnSync(process.execPath,['-e',script,require.resolve('./performance-v12.cjs'),differentlyCased],{encoding:'utf8',windowsHide:true,timeout:1500,killSignal:'SIGKILL'});
    assert.equal(result.error,undefined,'ancestor traversal must finish: '+result.error);
    assert.equal(result.status,0,result.stderr);
    assert.equal(fs.statSync(output).isDirectory(),true);
  }finally{if(fs.existsSync(output))fs.rmdirSync(output);}
});
test('mixed fixture persists exact raw counts and matching daily totals for both schemas',()=>{
  for(const category of [false,true]){
    const dir=fs.mkdtempSync(path.join(os.tmpdir(),'pulse-performance-unit-')),file=path.join(dir,'fixture.db');
    let db=new DatabaseSync(file);
    db.exec("CREATE TABLE checks(id INTEGER PRIMARY KEY,target_id INTEGER,input_url TEXT,final_url TEXT,status_code INTEGER,duration_ms REAL,kind TEXT,detail TEXT,checked_at INTEGER,total_duration_ms REAL,config_version INTEGER,rules_snapshot TEXT"+(category?',error_category TEXT':'')+"); CREATE TABLE daily_summaries(target_id INTEGER,date TEXT,samples INTEGER,successes INTEGER,duration_sum_ms REAL,max_ms REAL,migration_partial INTEGER); PRAGMA user_version="+(category?4:3));db.close();
    try{
      const fixture=populateStorage(file,2,30);assert.equal(fixture.total_checks,60);assert.equal(fixture.utc_days,30);
      db=new DatabaseSync(file);assert.equal(db.prepare('SELECT COUNT(*) n FROM checks').get().n,60);assert.equal(db.prepare('SELECT COUNT(*) n FROM daily_summaries').get().n,60);assert.equal(db.prepare('SELECT SUM(samples) n FROM daily_summaries').get().n,60);assert.equal(db.prepare('SELECT SUM(successes) n FROM daily_summaries').get().n,12);assert.deepEqual(db.prepare('SELECT kind,COUNT(*) n FROM checks GROUP BY kind').all().map(r=>r.n),[12,12,12,12,12]);db.close();db=null;
    }finally{db?.close();fs.rmSync(dir,{recursive:true,force:true});}
  }
});
