const {defineConfig}=require('@playwright/test');
const browser=process.env.PULSE_BROWSER||'chromium';
const browsers={
 chromium:{browserName:'chromium'},
 firefox:{browserName:'firefox'},
 edge:{browserName:'chromium',channel:'msedge'},
};
if(!Object.hasOwn(browsers,browser))throw new Error('PULSE_BROWSER must be chromium, firefox or edge');
module.exports=defineConfig({
 testDir:'./e2e',workers:1,fullyParallel:false,timeout:120000,
 expect:{timeout:15000},retries:0,reporter:[['list'],['html',{open:'never'}]],
 projects:[{name:browser,use:browsers[browser]}],
 use:{headless:true,trace:'retain-on-failure',screenshot:'only-on-failure'},
});
