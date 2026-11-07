const {test,expect}=require('@playwright/test');
const {launchServer,stopChild}=require('../scripts/launch-server.cjs');
const http=require('node:http');
const fs=require('node:fs/promises');
const os=require('node:os');
const path=require('node:path');
const {once}=require('node:events');
let upstream,upstreamURL,app,base,directory,db;
async function launch(address='127.0.0.1:0'){
 const exe=path.resolve('.cache',process.platform==='win32'?'pulse-e2e.exe':'pulse-e2e');
 return launchServer(exe,['serve','-addr',address,'-db',db]);
}
async function stop(){await stopChild(app?.child);}
test.beforeEach(async({page})=>{
 directory=await fs.mkdtemp(path.join(os.tmpdir(),'pulse-e2e-'));db=path.join(directory,'monitor.db');
 upstream=http.createServer((req,res)=>{
  if(req.url==='/slow'){const timer=setTimeout(()=>res.end('slow'),2000);res.on('close',()=>clearTimeout(timer));}
  else if(req.url==='/error')res.writeHead(500).end('error');
  else if(req.url==='/json')res.end('{"code":1}');
  else if(req.url==='/redirect')res.writeHead(302,{Location:'/ok'}).end();
  else res.end('ok');
 });
 upstream.listen(0,'127.0.0.1');await once(upstream,'listening');upstreamURL='http://127.0.0.1:'+upstream.address().port;
 app=await launch();base=app.url;await page.goto(base);await expect(page.getByText('还没有监测目标',{exact:true})).toBeVisible();
});
test.afterEach(async()=>{await stop();if(upstream){await new Promise(resolve=>upstream.close(resolve));}if(directory)await fs.rm(directory,{recursive:true,force:true});});
const row=(page,name)=>page.locator('#targets tr').filter({hasText:name});
async function list(request){const response=await request.get(base+'/api/targets');expect(response.ok()).toBeTruthy();return response.json();}
async function refresh(page){await page.locator('#refresh').click();await expect(page.locator('#refresh')).toBeEnabled();}
async function add(page,name,route,enabled=true){
 await page.locator('#add-target').click();
 await page.getByLabel('目标名称',{exact:true}).fill(name);await page.getByLabel('网址',{exact:true}).fill(upstreamURL+route);
 await page.getByLabel('检查间隔（秒）',{exact:true}).fill('5');await page.getByLabel('超时上限（秒）',{exact:true}).fill('1');
 await page.getByLabel('保存后启用自动监测').setChecked(enabled);await page.locator('#save-target').click();await expect(page.locator('#target-dialog')).not.toBeVisible();await expect(row(page,name).getByRole('button',{name:'编辑',exact:true})).toBeEnabled();
}
test('保存后的刷新未完成时添加按钮禁用，完成后能打开表单',async({page})=>{
 await add(page,'操作时序目标','/ok',false);
 let release;const held=new Promise(resolve=>release=resolve);
 await page.route(/\/api\/targets$/,async route=>{if(route.request().method()==='GET')await held;await route.continue();});
 const requested=page.waitForRequest(req=>req.method()==='GET'&&req.url().endsWith('/api/targets'));
 await row(page,'操作时序目标').getByRole('button',{name:'恢复',exact:true}).click();await requested;
 try{await expect(page.locator('#add-target')).toBeDisabled();}finally{release();}
 await expect(page.locator('#add-target')).toBeEnabled();await page.locator('#add-target').click();await expect(page.locator('#target-dialog')).toBeVisible();
});

test('完整管理流程、校验、异常和筛选',async({page,request})=>{
 await page.locator('#add-target').click();await page.locator('#save-target').click();
 expect(await page.locator('input[name=name]').evaluate(e=>e.validity.valueMissing)).toBeTruthy();
 await page.getByLabel('目标名称',{exact:true}).fill('   ');await page.getByLabel('网址',{exact:true}).fill(upstreamURL+'/ok');await page.locator('#save-target').click();await expect(page.locator('#form-error')).toContainText('名称');
 await page.getByRole('button',{name:'取消',exact:true}).click();
 await add(page,'正常目标','/ok');await expect.poll(async()=> (await list(request))[0].latest?.kind).toBe('ok');await refresh(page);await expect(row(page,'正常目标')).toContainText('正常');
 await row(page,'正常目标').getByRole('button',{name:'编辑',exact:true}).click();await page.getByLabel('目标名称',{exact:true}).fill('编辑后目标');await page.getByLabel('网址',{exact:true}).fill(upstreamURL+'/redirect');await page.locator('#save-target').click();await expect(row(page,'编辑后目标')).toContainText('/redirect');
 await row(page,'编辑后目标').getByRole('button',{name:'暂停',exact:true}).click();await expect(row(page,'编辑后目标')).toContainText('已暂停');
 const before=(await list(request))[0].stats.samples;
 await row(page,'编辑后目标').getByRole('button',{name:'立即检查',exact:true}).click();await expect.poll(async()=> (await list(request))[0].stats.samples).toBeGreaterThan(before);
 expect((await list(request))[0].enabled).toBe(false);
 await row(page,'编辑后目标').getByRole('button',{name:'历史',exact:true}).click();await expect(page.locator('#history-rows')).toContainText('最终地址：'+upstreamURL+'/ok');await expect(page.locator('#history-rows')).toContainText(upstreamURL+'/ok');await page.getByRole('button',{name:'完成',exact:true}).click();
 await row(page,'编辑后目标').getByRole('button',{name:'恢复',exact:true}).click();await expect.poll(async()=> (await list(request))[0].enabled).toBe(true);
 await add(page,'错误目标','/error');await add(page,'超时目标','/slow');await expect.poll(async()=> (await list(request)).find(t=>t.name==='超时目标').latest?.kind).toBe('timeout');await refresh(page);
 await expect(row(page,'错误目标')).toContainText('HTTP 异常');await expect(row(page,'超时目标')).toContainText('超时');
 await page.locator('#search').fill('不存在的名字');await expect(page.locator('#empty-title')).toHaveText('没有匹配的监测目标');await page.locator('#search').fill('错误目标');await expect(page.locator('#targets tr')).toHaveCount(1);await page.locator('#search').fill('');
 await page.locator('#filter').selectOption('error');await expect(page.locator('#targets tr')).toHaveCount(2);await page.locator('#filter').selectOption('healthy');await expect(page.locator('#targets tr')).toHaveCount(1);
 await row(page,'编辑后目标').getByRole('button',{name:'暂停',exact:true}).click();await page.locator('#filter').selectOption('paused');await expect(page.locator('#targets tr')).toHaveCount(1);
});
test('55条历史分页、重开、切换、删除及手机布局',async({page,request},info)=>{
 await add(page,'分页目标','/ok',false);await row(page,'分页目标').getByRole('button',{name:'历史',exact:true}).click();await expect(page.locator('#history-empty')).toBeVisible();await page.getByRole('button',{name:'完成',exact:true}).click();
 const id=(await list(request))[0].id;
 for(let n=1;n<=55;n++){
  await expect.poll(async()=>{const r=await request.post(base+`/api/targets/${id}/check`);return r.status();}).toBe(202);
  await expect.poll(async()=>{const t=(await list(request))[0];return t.stats.samples>=n&&!t.running;}).toBe(true);
 }
 await refresh(page);await page.getByRole('button',{name:'分页目标',exact:true}).click();await expect(page.locator('#history-rows tr')).toHaveCount(50);await expect(page.locator('#history-chart circle')).toHaveCount(30);await page.locator('#history-more').click();await expect(page.locator('#history-rows tr')).toHaveCount(55);await expect(page.locator('#history-more')).toBeHidden();
 await page.getByRole('button',{name:'完成',exact:true}).click();await page.getByRole('button',{name:'分页目标',exact:true}).click();await expect(page.locator('#history-rows tr')).toHaveCount(50);await page.getByRole('button',{name:'完成',exact:true}).click();
 await add(page,'空历史目标','/ok',false);await page.getByRole('button',{name:'空历史目标',exact:true}).click();await expect(page.locator('#history-empty')).toBeVisible();await expect(page.locator('#history-rows tr')).toHaveCount(0);await page.getByRole('button',{name:'完成',exact:true}).click();
 for(const width of [390,320]){
  await page.setViewportSize({width,height:844});expect(await page.evaluate(()=>document.documentElement.scrollWidth<=document.documentElement.clientWidth)).toBe(true);
  await page.getByRole('button',{name:'分页目标',exact:true}).click();await expect(page.locator('#history-rows tr')).toHaveCount(50);
  expect(await page.locator('#history-dialog').evaluate(d=>{const r=d.getBoundingClientRect();return r.left>=0&&r.right<=document.documentElement.clientWidth;})).toBe(true);
  await page.screenshot({path:info.outputPath(`mobile-${width}.png`)});await page.getByRole('button',{name:'完成',exact:true}).click();
 }
 await row(page,'分页目标').getByRole('button',{name:'删除',exact:true}).click();await page.locator('#delete-dialog').getByRole('button',{name:'取消',exact:true}).click();await expect(row(page,'分页目标')).toHaveCount(1);
 await row(page,'分页目标').getByRole('button',{name:'删除',exact:true}).click();await page.locator('#confirm-delete').click();await expect(row(page,'分页目标')).toHaveCount(0);expect((await request.get(base+`/api/targets/${id}/checks`)).status()).toBe(404);
});
test('真实服务断开、页面保留数据、同库重启恢复',async({page,request})=>{
 await add(page,'持久化目标','/ok',false);await stop();await refresh(page);await expect(page.locator('#global-error')).toContainText('已有数据保留');await expect(row(page,'持久化目标')).toHaveCount(1);
 app=await launch(new URL(base).host);await refresh(page);await expect(page.locator('#global-error')).toBeHidden();await expect(row(page,'持久化目标')).toHaveCount(1);expect((await list(request))[0].name).toBe('持久化目标');
});

test('业务规则、精确值编辑与UTC日汇总',async({page,request},info)=>{
 await add(page,'业务目标','/json',false);
 await row(page,'业务目标').getByRole('button',{name:'编辑',exact:true}).click();
 await page.getByLabel('允许状态码',{exact:true}).fill('200');
 await page.getByLabel('正文包含',{exact:true}).fill('code');
 await page.getByRole('button',{name:'添加JSON规则',exact:true}).click();
 await page.getByLabel('JSON路径 1',{exact:true}).fill('/code');
 await page.getByLabel('期望值 1',{exact:true}).fill('0');
 await page.locator('#save-target').click();await expect(page.locator('#target-dialog')).not.toBeVisible();await expect(row(page,'业务目标').getByRole('button',{name:'立即检查',exact:true})).toBeEnabled();
 await row(page,'业务目标').getByRole('button',{name:'立即检查',exact:true}).click();
 await expect.poll(async()=> (await list(request))[0].latest?.kind).toBe('assertion_failed');await refresh(page);
 await row(page,'业务目标').getByRole('button',{name:'历史',exact:true}).click();
 await expect(page.locator('#history-rows')).toContainText('值或类型不匹配');await expect(page.locator('#daily-rows tr')).toHaveCount(30);await expect(page.locator('#daily-rows')).toContainText('无样本');await expect(page.locator('#daily-note')).toContainText('UTC');
 await page.screenshot({path:info.outputPath('daily-desktop.png')});
 await page.getByRole('button',{name:'完成',exact:true}).click();
 await row(page,'业务目标').getByRole('button',{name:'编辑',exact:true}).click();await expect(page.getByLabel('期望值 1',{exact:true})).toHaveValue('0');
 await page.getByLabel('期望值 1',{exact:true}).fill('1');await page.locator('#save-target').click();await expect(page.locator('#target-dialog')).not.toBeVisible();await expect(row(page,'业务目标').getByRole('button',{name:'立即检查',exact:true})).toBeEnabled();
 await row(page,'业务目标').getByRole('button',{name:'立即检查',exact:true}).click();await expect.poll(async()=> (await list(request))[0].latest?.kind).toBe('ok');
 const t=(await list(request))[0];await request.put(base+'/api/targets/'+t.id,{data:{name:t.name,url:t.url,interval_seconds:5,timeout_seconds:1,enabled:false}});expect((await list(request))[0].rules.json_equals[0].equals).toBe(1);
 await refresh(page);await row(page,'业务目标').getByRole('button',{name:'编辑',exact:true}).click();
 await page.getByLabel('期望值 1',{exact:true}).fill('9007199254740993');await page.locator('#save-target').click();await expect(page.locator('#target-dialog')).not.toBeVisible();await expect(row(page,'业务目标').getByRole('button',{name:'编辑',exact:true})).toBeEnabled();
 await row(page,'业务目标').getByRole('button',{name:'编辑',exact:true}).click();await expect(page.getByLabel('期望值 1',{exact:true})).toHaveValue('9007199254740993');
 for(let i=0;i<4;i++)await page.getByRole('button',{name:'添加JSON规则',exact:true}).click();await expect(page.getByRole('button',{name:'添加JSON规则',exact:true})).toBeDisabled();await expect(page.getByLabel('期望值 1',{exact:true})).toHaveValue('9007199254740993');
 await page.setViewportSize({width:320,height:844});expect(await page.evaluate(()=>document.documentElement.scrollWidth<=document.documentElement.clientWidth)).toBe(true);await page.screenshot({path:info.outputPath('rules-mobile.png')});
});

test('编辑名称保留正文和JSON路径中的LF与CRLF',async({page,request})=>{
 for(const special of ['\n','\r\n']){
  const name='特殊字符'+JSON.stringify(special);
  const rules={body_contains:special,json_equals:[{pointer:'/line'+special+'key',equals:0}]};
  const response=await request.post(base+'/api/targets',{data:{name,url:upstreamURL+'/ok',interval_seconds:60,timeout_seconds:1,enabled:false,rules}});expect(response.ok()).toBeTruthy();
  const created=await response.json();await refresh(page);
  await row(page,name).getByRole('button',{name:'编辑',exact:true}).click();await page.getByLabel('目标名称',{exact:true}).fill(name+'改名');await page.locator('#save-target').click();await expect(page.locator('#target-dialog')).not.toBeVisible();
  expect((await list(request)).find(t=>t.id===created.id).rules).toEqual(rules);
 }
});

test('首版数据库升级后标注不完整汇总且重启不重复累计',async({page,request})=>{
 await stop();for(const suffix of ['', '-wal','-shm'])await fs.rm(db+suffix,{force:true});
 const {DatabaseSync}=require('node:sqlite');const legacy=new DatabaseSync(db);
 legacy.exec(`CREATE TABLE targets(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,url TEXT NOT NULL,interval_seconds INTEGER NOT NULL,timeout_seconds INTEGER NOT NULL,enabled INTEGER NOT NULL,created_at INTEGER NOT NULL,next_check_at INTEGER NOT NULL,version INTEGER NOT NULL DEFAULT 1);
 CREATE TABLE checks(id INTEGER PRIMARY KEY AUTOINCREMENT,target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,input_url TEXT NOT NULL,final_url TEXT NOT NULL,status_code INTEGER NOT NULL,duration_ms REAL NOT NULL,kind TEXT NOT NULL,detail TEXT NOT NULL,checked_at INTEGER NOT NULL);CREATE INDEX checks_target_id ON checks(target_id,id DESC);PRAGMA user_version=1;`);
 const now=Date.now();legacy.prepare('INSERT INTO targets VALUES(1,?,?,60,1,0,?,?,1)').run('迁移目标',upstreamURL+'/ok',now,now);legacy.prepare('INSERT INTO checks VALUES(1,1,?,?,200,12,\'ok\',\'旧样本\',?)').run(upstreamURL+'/ok',upstreamURL+'/ok',now);legacy.close();
 app=await launch(new URL(base).host);await refresh(page);await row(page,'迁移目标').getByRole('button',{name:'历史',exact:true}).click();await expect(page.locator('#daily-rows')).toContainText('迁移前历史不完整');
 const daily=await (await request.get(base+'/api/targets/1/daily?days=1')).json();expect(daily.buckets[0].samples).toBe(1);expect((await list(request))[0].latest.total_duration_ms).toBeNull();await page.getByRole('button',{name:'完成',exact:true}).click();
 await stop();app=await launch(new URL(base).host);const reopened=await (await request.get(base+'/api/targets/1/daily?days=1')).json();expect(reopened.buckets[0].samples).toBe(1);
});
