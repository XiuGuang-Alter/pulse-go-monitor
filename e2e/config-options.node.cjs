const test=require('node:test');
const assert=require('node:assert/strict');
const {spawnSync}=require('node:child_process');
const path=require('node:path');

function loadConfig(browser){
 const env={...process.env};
 if(browser===undefined)delete env.PULSE_BROWSER;else env.PULSE_BROWSER=browser;
 return spawnSync(process.execPath,['-e',`const c=require('./playwright.config.cjs');process.stdout.write(JSON.stringify({projects:c.projects,retries:c.retries}));`],{
  cwd:path.resolve(__dirname,'..'),env,encoding:'utf8',
 });
}

for(const [value,name,browserName,channel] of [
 [undefined,'chromium','chromium',undefined],
 ['chromium','chromium','chromium',undefined],
 ['firefox','firefox','firefox',undefined],
 ['edge','edge','chromium','msedge'],
])test(`PULSE_BROWSER=${value??'(unset)'} selects only ${name}`,()=>{
 const result=loadConfig(value);assert.equal(result.status,0,result.stderr);
 const config=JSON.parse(result.stdout);
 assert.equal(config.projects?.length,1,'default CI must never expand to multiple browsers');
 assert.equal(config.projects[0].name,name);
 assert.equal(config.projects[0].use.browserName,browserName);
 assert.equal(config.projects[0].use.channel,channel);
 assert.equal(config.retries,0,'first failures must remain visible');
});

test('unknown PULSE_BROWSER is rejected before running a different browser',()=>{
 const result=loadConfig('safari');
 assert.notEqual(result.status,0);
 assert.match(result.stderr,/PULSE_BROWSER.*chromium.*firefox.*edge/);
});
