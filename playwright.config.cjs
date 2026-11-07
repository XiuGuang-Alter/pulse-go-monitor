const {defineConfig}=require('@playwright/test');
module.exports=defineConfig({
 testDir:'./e2e',workers:1,fullyParallel:false,timeout:120000,
 expect:{timeout:15000},retries:0,reporter:[['list'],['html',{open:'never'}]],
 use:{browserName:'chromium',headless:true,trace:'retain-on-failure',screenshot:'only-on-failure'},
});
