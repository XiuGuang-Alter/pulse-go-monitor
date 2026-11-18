const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

function setup(fetchEvents) {
 const elements=new Map();
 const element=()=>({children:[],listeners:{},value:'0',hidden:false,disabled:false,textContent:'',classList:{add(){},remove(){},toggle(){}},
  append(...nodes){this.children.push(...nodes)},replaceChildren(...nodes){this.children=[...nodes]},addEventListener(k,f){this.listeners[k]=f},setAttribute(){},showModal(){},close(){},reset(){}});
 const document={hidden:false,getElementById(id){if(!elements.has(id))elements.set(id,element());return elements.get(id)},querySelectorAll(){return []},createElement:element,createElementNS:element,createTextNode:element};
 const context=vm.createContext({document,console,AbortSignal,URLSearchParams,setInterval(){},setTimeout(){},clearTimeout(){},fetch:async url=>({ok:true,status:200,json:async()=>url.startsWith('/api/events')?fetchEvents(url):url==='/api/targets'?[]:{running:0,concurrency:4}})});
 vm.runInContext(fs.readFileSync(require('node:path').join(__dirname,'web/app.js'),'utf8'),context);
 return {context,elements,run:code=>vm.runInContext(code,context)};
}
const event=(id,name='target')=>({id,target_id:1,target_name:name,config_version:2,occurred_at:Date.now(),kind:'fault',notification_status:'disabled',error_category:'timeout'});

test('提醒记录分页、安全文本和失败后保留已有记录',async()=>{
 let fail=false;const urls=[];
 const h=setup(url=>{urls.push(url);if(fail)throw Error('模拟断网');return url.includes('before_id=')?[event(1)]:Array.from({length:50},(_,i)=>event(100-i,i===0?'<img src=x onerror=alert(1)>':'target'));});
 await h.run('openEvents()');
 assert.equal(h.run('state.eventRows.length'),50);
 assert.equal(h.elements.get('event-rows').children[0].children[1].textContent,'<img src=x onerror=alert(1)>');
 await h.run('loadEvents(true)');assert.equal(h.run('state.eventRows.length'),51);assert.match(urls.at(-1),/before_id=51/);
 fail=true;await h.run('loadEvents(true)');assert.equal(h.run('state.eventRows.length'),51);assert.match(h.elements.get('event-error').textContent,/模拟断网/);
});

test('旧筛选响应和关闭后的响应不能覆盖当前提醒记录',async()=>{
 let release;let delayed=false;
 const h=setup(url=>delayed?new Promise(r=>release=r):[event(url.includes('target_id=2')?2:1)]);
 await h.run('openEvents()');delayed=true;const old=h.run('loadEvents(false)');await new Promise(r=>setImmediate(r));delayed=false;
 h.run('state.eventTarget=2; state.eventToken++; state.eventBusy=false');await h.run('loadEvents(false)');release([event(999)]);await old;
 assert.equal(h.run('state.eventRows[0].id'),2);
 delayed=true;const closing=h.run('loadEvents(false)');await new Promise(r=>setImmediate(r));h.elements.get('events-dialog').listeners.close();release([event(888)]);await closing;
 assert.equal(h.run('state.eventRows[0].id'),2);
});
