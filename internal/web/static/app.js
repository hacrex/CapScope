"use strict";
const $=s=>document.querySelector(s);
const ROW_H=20,MAXROWS=100000,MAXPANES=4;
function showLogin(){$('#loginOverlay').style.display='flex';}
function hideLogin(){$('#loginOverlay').style.display='none';}
const S={app:null,tab:'dashboard',wrapLines:false,jsonPretty:false,groupLines:localStorage.getItem('groupLines')!=='0',tz:localStorage.getItem('tz')||'local',collapseDupes:localStorage.getItem('collapseDupes')!=='0',range:'1h',host:'',hosts:[],accStatus:'',alertStatus:new Map(),alertMuted:new Map(),hl:new Set(),sel:new Set(),selKeys:new Set(),selData:new Map(),lastSelKey:null,selCont:null,panes:[],conts:[],apps:[],theme:localStorage.getItem('theme')||'auto',appCrashes:new Map(),sideNarrow:false,sideMode:localStorage.getItem('sideMode')==='apps'?'apps':'containers',sessionExpires:0,user:'anonymous',role:'viewer',authEnabled:true};

/* ---------- theme ---------- */
const THEMES=['auto','dark','light'];
const THEME_LABEL={auto:'Auto',dark:'Dark',light:'Light'};
function applyTheme(t){
  if(t==='auto')document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme',t);
  $('#themeBtn').title='Theme: '+(THEME_LABEL[t]||'Auto')+' — click to cycle';
}
applyTheme(S.theme);
$('#themeBtn').onclick=()=>{
  S.theme=THEMES[(THEMES.indexOf(S.theme)+1)%THEMES.length];
  localStorage.setItem('theme',S.theme);
  applyTheme(S.theme);
};
/* Time display: browser-local or UTC (survives reload; panes re-render lazily). */
function applyTz(){
  $('#tzBtn').title='Times shown in '+(S.tz==='utc'?'UTC':'browser local')+' — click to switch';
  for(const p of S.panes)if(p.vl&&p.vl.rows)p.vl.schedule(); // repaint visible rows only
}
applyTz();
$('#tzBtn').onclick=()=>{
  S.tz=S.tz==='utc'?'local':'utc';
  localStorage.setItem('tz',S.tz);
  applyTz();
  toast('Times: '+(S.tz==='utc'?'UTC':'browser local'));
};
$('#shareBtn').onclick=()=>copyShareLink(7);

/* ---------- ⌘K command palette ---------- */
// v0.7 fix: TABS/RANGE_MS/selectApp are declared later (const TDZ / function
// hoisting) — paletteActions() can run on Ctrl+K before the whole script has
// executed. Guard with try/catch so opening the palette never throws.
function safeAct(grp,label,run){return {grp,label,run:()=>{try{run();}catch(e){console.warn('palette action not ready:',e.message);}}};}
/* Fuzzy-ish subsequence matcher: every query char must appear in order. */
function fuzzyMatch(text,q){
  const t=text.toLowerCase(),s=q.toLowerCase();
  if(!s)return 0;
  let i=0,score=0,prev=-1;
  for(let j=0;j<t.length&&i<s.length;j++){
    if(t[j]===s[i]){score+=(j===prev+1?2:1);if(j===i)score+=3;prev=j;i++;}
  }
  return i===s.length?score:-1;
}
let paletteOpen=false,paletteSel=0,paletteItems=[];
/* TABS/RANGE_MS are declared later in the script; paletteActions() runs only on
   keypress (after full parse), so referencing them lazily is safe. */
function paletteActions(){
  const acts=[
    ...TABS.map(t=>safeAct('Tabs','Go to '+t.charAt(0).toUpperCase()+t.slice(1),()=>setTab(t))),
    ...Object.keys(RANGE_MS).map(r=>safeAct('Range','Time range: '+r,()=>{S.range=r;setRangeChip();syncURL();show();})),
    safeAct('View','Toggle wrap lines',()=>{$('#wrap')?.click();}),
    safeAct('View','Toggle pretty-print JSON',()=>{$('#jsonBtn')?.click();}),
    safeAct('View',(S.theme==='dark'?'Switch to light theme':'Switch to dark theme'),()=>{S.theme=S.theme==='dark'?'light':'dark';localStorage.setItem('theme',S.theme);applyTheme(S.theme);toast('Theme: '+THEME_LABEL[S.theme]);}),
    safeAct('View','Collapse duplicate log lines: '+(S.collapseDupes?'on':'off'),()=>{S.collapseDupes=!S.collapseDupes;localStorage.setItem('collapseDupes',S.collapseDupes?'1':'0');toast('Pattern collapse '+(S.collapseDupes?'enabled':'disabled'));for(const p of S.panes)if(p.vl&&p.vl.rows)p.vl.reset(p.vl.rows);}),
    safeAct('View','Fold stack-trace lines (multi-line): '+(S.groupLines?'on':'off'),()=>{$('#group').click();}),
    safeAct('View',(S.tz==='utc'?'Show times in browser local':'Show times in UTC'),()=>$('#tzBtn').click()),
    safeAct('Share','Copy expiring share link for this view (7 days)',()=>copyShareLink(7)),
    safeAct('View','Toggle sidebar: '+(S.sideNarrow?'collapsed':'expanded'),()=>setSidebar(!S.sideNarrow)),
    safeAct('Alerts','New alert rule',()=>{setTab('alerts');setTimeout(()=>document.querySelectorAll('#view-alerts button').forEach(b=>{if(b.textContent==='+ New rule')b.click();}),80);}),
    safeAct('View','Jump to Crashes tab',()=>setTab('crashes')),
  ];
  for(const a of S.apps)acts.push(safeAct('Apps','Open app: '+a.app,()=>selectApp(a.app)));
  return acts;
}
function renderPalette(){
  const q=$('#paletteInput').value.trim();
  const scored=paletteActions()
    .map(a=>({a,score:fuzzyMatch(a.label,q)}))
    .filter(x=>x.score>=0)
    .sort((x,y)=>y.score-x.score)
    .slice(0,40)
    .map(x=>x.a);
  paletteItems=scored;paletteSel=0;
  const list=$('#paletteList');list.innerHTML='';
  if(!scored.length){const e=document.createElement('div');e.className='empty';e.style.padding='16px';e.textContent='No matches';list.append(e);return;}
  scored.forEach((a,i)=>{
    const it=document.createElement('div');it.className='pitem'+(i===0?' sel':'');
    const d=document.createElement('span');d.className='dot';d.style.background=a.grp==='Apps'?'var(--accent)':a.grp==='Tabs'?'var(--ok)':'var(--muted)';
    const l=document.createElement('span');l.textContent=a.label;
    const g=document.createElement('span');g.className='grp';g.textContent=a.grp;
    it.append(d,l,g);
    it.onmouseenter=()=>{paletteSel=i;highlightPalette();};
    it.onclick=()=>runPalette();
    list.append(it);
  });
}
function highlightPalette(){
  document.querySelectorAll('#paletteList .pitem').forEach((el,i)=>el.classList.toggle('sel',i===paletteSel));
  const selEl=document.querySelector('#paletteList .pitem.sel');
  if(selEl)selEl.scrollIntoView({block:'nearest'});
}
function runPalette(){
  const a=paletteItems[paletteSel];
  closePalette();
  if(a)try{a.run();}catch(e){console.warn('palette action failed',e);}
}
function openPalette(){
  paletteOpen=true;
  $('#paletteOverlay').style.display='flex';
  $('#paletteInput').value='';
  renderPalette();
  $('#paletteInput').focus();
}
function closePalette(){paletteOpen=false;$('#paletteOverlay').style.display='none';}
$('#paletteInput').oninput=renderPalette;
$('#paletteInput').onkeydown=e=>{
  if(e.key==='ArrowDown'){e.preventDefault();paletteSel=Math.min(paletteSel+1,paletteItems.length-1);highlightPalette();}
  else if(e.key==='ArrowUp'){e.preventDefault();paletteSel=Math.max(paletteSel-1,0);highlightPalette();}
  else if(e.key==='Enter'){e.preventDefault();runPalette();}
};
$('#paletteOverlay').onclick=e=>{if(e.target.id==='paletteOverlay')closePalette();};
/* the header search box opens the palette (it is readonly: typing happens inside) */
$('#searchbox').onclick=()=>openPalette();
addEventListener('keydown',e=>{
  if((e.ctrlKey||e.metaKey)&&e.key.toLowerCase()==='k'){e.preventDefault();paletteOpen?closePalette():openPalette();return;}
  if(!paletteOpen)return;
  if(e.key==='Escape'){e.preventDefault();closePalette();}
});

async function api(path,init){
  const opts=Object.assign({cache:'no-store'},init||{});
  const h=new Headers(opts.headers||{});
  const t=localStorage.getItem('capscope_token');
  if(t)h.set('Authorization','Bearer '+t);
  opts.headers=h;
  const r=await fetch(path,opts);
  if(r.status===401){localStorage.removeItem('capscope_token');showLogin();throw new Error('unauthorized');}
  if(!r.ok)throw new Error(await r.text());
  return r.json();
}
const fmtTime=ms=>{const d=new Date(ms);if(S.tz==='utc'){return String(d.getUTCHours()).padStart(2,'0')+':'+String(d.getUTCMinutes()).padStart(2,'0')+':'+String(d.getUTCSeconds()).padStart(2,'0')+'.'+String(d.getUTCMilliseconds()).padStart(3,'0');}return d.toTimeString().slice(0,8)+'.'+String(d.getMilliseconds()).padStart(3,'0');};
const fmtDT=ms=>{const d=new Date(ms);return S.tz==='utc'?d.toISOString().replace('T',' ').slice(0,19)+' UTC':d.toLocaleString();};
const mib=b=>(b/1048576).toFixed(b>1073741824?0:1)+' MiB';
const appIcon=image=>{
  const i=(image||'').toLowerCase();
  if(i.includes('nginx'))return'🟢';
  if(i.includes('apache')||i.includes('httpd'))return'🟠';
  if(i.includes('postgres'))return'🟣';
  if(i.includes('mysql')||i.includes('mariadb'))return'🔵';
  if(i.includes('redis'))return'🔴';
  if(i.includes('mongo'))return'🟢';
  if(i.includes('rabbitmq'))return'🟣';
  if(i.includes('kafka'))return'🟠';
  if(i.includes('elasticsearch')||i.includes('opensearch'))return'🟡';
  if(i.includes('prometheus'))return'🟢';
  if(i.includes('grafana'))return'🟠';
  if(i.includes('docker')||i.includes('portainer'))return'🐳';
  if(i.includes('node'))return'🟢';
  if(i.includes('python'))return'🟡';
  if(i.includes('golang')||i.includes('go:'))return'🔵';
  if(i.includes('java')||i.includes('tomcat'))return'☕';
  return'📦';
};
function ago(ms){const s=(Date.now()-ms)/1000;if(s<60)return Math.round(s)+'s ago';if(s<3600)return Math.round(s/60)+'m ago';if(s<86400)return Math.round(s/3600)+'h ago';return Math.round(s/86400)+'d ago';}
/* Live-ticking ages: rewrite only the text of [data-ago] / [data-ago-ts]
   elements instead of re-rendering the view, so relative times ("3m ago")
   stay honest between polls without touching the DOM structure. */
setInterval(()=>{
  document.querySelectorAll('[data-ago]').forEach(el=>{el.textContent=ago(+el.dataset.ago);});
  document.querySelectorAll('[data-ago-ts]').forEach(el=>{const ts=+el.dataset.agoTs;if(isFinite(ts))el.textContent=fmtDT(ts)+' ('+ago(ts)+')';});
},15000);
/* Surface a transient failure on the connection chip; refreshApps restores the real state. */
let badT;
function connBad(msg){const c=$('#conn');c.textContent=msg;c.className='chip bad';clearTimeout(badT);badT=setTimeout(refreshApps,4000);}
/* transient connection states (reconnecting / dropped rows) must not trigger
   the connBad auto-refresh timer — chips are informational only. */
function connChip(kind,msg){const c=$('#conn');c.textContent=msg;c.className='chip '+kind;clearTimeout(badT);}
/* ---------- toasts ---------- */
/* Non-blocking feedback for saves/records; replaces inline status text on success. */
function toast(msg,kind,undo){
  const box=$('#toasts');if(!box)return;
  const t=document.createElement('div');t.className='toast'+(kind?' '+kind:'');
  const m=document.createElement('span');m.textContent=msg;t.append(m);
  let dead=false;
  const kill=()=>{if(dead)return;dead=true;t.style.transition='opacity .3s';t.style.opacity='0';setTimeout(()=>t.remove(),350);};
  if(typeof undo==='function'){
    const u=document.createElement('button');u.className='undo';u.type='button';u.textContent='Undo';
    u.onclick=()=>{kill();try{undo();}catch(e){console.warn('capscope: undo failed',e);}};
    t.append(u);
  }
  box.append(t);
  setTimeout(kill,undo?7000:4000);
}
/* Clipboard helper with toast feedback (row + line hover actions). */
function copyText(s,msg){
  if(!s)return;
  navigator.clipboard.writeText(s).then(()=>toast(msg||'Copied')).catch(()=>toast('Copy failed','err'));
}
/* Value flash: highlight a number when it changes between dashboard refreshes.
   First sighting only records the baseline so the initial paint stays quiet. */
const lastVals={};
function flashVal(el,key){
  const v=el.textContent;
  if(lastVals[key]!==undefined&&lastVals[key]!==v){
    el.classList.remove('flash');void el.offsetWidth;el.classList.add('flash');
  }
  lastVals[key]=v;
}

/* Keep the Logs-tab CSV download in step with the current filters. */
function syncLogCSV(){$('#logCSV').href='/api/logs/export?'+params({},null);$('#logJSON').href='/api/logs/export?fmt=json&'+params({},null);$('#logTXT').href='/api/logs/raw?'+params({},null);}
function escCsv(v){const s=String(v);return /[",\n]/.test(s)?'"'+s.replace(/"/g,'""')+'"':s;}
function downloadSelected(){
  const rows=[];document.querySelectorAll('#logs .ln.sel,.plog .ln.sel').forEach(d=>{
    rows.push([d.dataset.ts||'',d.dataset.container||'',d.dataset.line||'']);
  });
  if(!rows.length)return;
  let csv='ts,container,line\n'+rows.map(r=>r.map(escCsv).join(',')).join('\n');
  const blob=new Blob([csv],{type:'text/csv;charset=utf-8'});
  const url=URL.createObjectURL(blob);const a=document.createElement('a');
  a.href=url;a.download=`capscope-selected-${new Date().toISOString().slice(0,16).replace(/[:-]/g,'')}.csv`;
  a.click();URL.revokeObjectURL(url);
}
$('#exportSel').onclick=downloadSelected;

/* ---------- sidebar ---------- */
async function refreshApps(){
  try{
    const [a,c]=await Promise.all([api('/api/apps'),api('/api/containers')]);
    S.conts=c.containers;S.apps=a.apps;
    if(S.selCont)S.selCont=S.conts.find(x=>x.name===S.selCont.name)||null;
    const live={};for(const x of S.conts){(live[x.app]=live[x.app]||[]).push(x);}
    const names=new Set([...a.apps.map(x=>x.app),...Object.keys(live)]);
    const byName=Object.fromEntries(a.apps.map(x=>[x.app,x]));
    const box=$('#apps');box.innerHTML='';
    if(!names.size&&!S.conts.length){
      const empty=document.createElement('div');empty.className='empty';
      empty.innerHTML='No containers found<br><span style="font-size:12px;opacity:0.7">Mount /var/run/docker.sock or check the EXCLUDE filter.</span>';
      box.append(empty);$('#conn').textContent='offline';$('#conn').className='chip bad';
      fillReplicas();fillMetricsContainers();updateContainerStat();return;
    }
    const appList=[...names].map(n=>{const h=byName[n]||{};return{name:n,errs:h.errors_1h||0,warns:h.warns_1h||0,crashes:S.appCrashes.get(n)||0,cs:(live[n]||[]).slice().sort((x,y)=>String(x.replica||x.name)<String(y.replica||y.name)?-1:1)};});
    appList.sort((x,y)=>(y.crashes-x.crashes)||(y.errs-x.errs)||(x.name<y.name?-1:x.name>y.name?1:0));
    if(S.sideMode==='containers')renderContainerList(box,appList,a);
    else renderAppList(box,appList,a);
    $('#conn').textContent=`${S.conts.length} containers`;$('#conn').className='chip live';
    if(S.app===null&&S.selCont===null&&appList.length&&S.tab!=='dashboard')selectApp(appList[0].name);
    markSidebar(); // selection may have changed above; keep rows in step
    applyAppFilter(); // re-apply any active sidebar filter after the rebuild
    fillReplicas();fillMetricsContainers();
    updateContainerStat();
  }catch(e){$('#conn').textContent='offline';$('#conn').className='chip bad';console.error('capscope: refreshApps failed',e);}
}
/* Sidebar, "Apps" mode: one row per app with resource meters and error badges. */
function renderAppList(box,appList,a){
  const row=(n,label,cs,cpu,mem,errs,warns,crashes)=>{
    const el=document.createElement('div');el.className='app';el.dataset.n=n;
    const nm=document.createElement('div');nm.className='n';
    const icon=cs[0]?.image?appIcon(cs[0].image):'📦';
    const t=document.createElement('span');t.textContent=icon+' '+label;nm.append(t);
    const b=document.createElement('span');
    if(crashes){const d=document.createElement('span');d.className='badge e';d.textContent=crashes+' crash'+(crashes>1?'es':'');d.title='crash snapshots in the last 24h';b.append(d,' ');}
    if(errs){const e=document.createElement('span');e.className='badge e';e.textContent=errs;e.title='errors in the last hour';b.append(e,' ');}
    if(warns){const w=document.createElement('span');w.className='badge w';w.textContent=warns;w.title='warnings in the last hour';b.append(w);}
    nm.append(b);
    const m=document.createElement('div');m.className='m';
    const totalMem=cs.reduce((t,x)=>t+x.mem_limit,0);
    const usedMem=cs.reduce((t,x)=>t+x.mem,0);
    const memPct=totalMem>0?usedMem/totalMem*100:0;
    m.textContent=cs.length?`${cs.length} running`:'not running';
    const meters=document.createElement('div');meters.className='meters';
    const cpuMeter=document.createElement('div');cpuMeter.className='app-meter cpu';
    const cpuVal=document.createElement('span');cpuVal.className='val';cpuVal.textContent=cpu.toFixed(0)+'%';
    const cpuTrack=document.createElement('div');cpuTrack.className='track';
    const cpuFill=document.createElement('div');cpuFill.className='fill';cpuFill.style.width=Math.min(100,Math.max(0,cpu))+'%';
    cpuTrack.append(cpuFill);cpuMeter.append(cpuVal,cpuTrack);
    const memMeter=document.createElement('div');memMeter.className='app-meter mem';
    const memVal=document.createElement('span');memVal.className='val';memVal.textContent=memPct.toFixed(0)+'%';
    const memTrack=document.createElement('div');memTrack.className='track';
    const memFill=document.createElement('div');memFill.className='fill';memFill.style.width=Math.min(100,Math.max(0,memPct))+'%';
    memTrack.append(memFill);memMeter.append(memVal,memTrack);
    meters.append(cpuMeter,memMeter);
    m.append(meters);
    el.append(nm,m);el.onclick=()=>selectApp(n);box.append(el);
  };
  // "All apps": one merged timeline over every app (V7)
  row('','All apps',S.conts,
    S.conts.reduce((t,x)=>t+x.cpu,0),S.conts.reduce((t,x)=>t+x.mem,0),
    a.apps.reduce((t,x)=>t+(x.errors_1h||0),0),a.apps.reduce((t,x)=>t+(x.warns_1h||0),0),S.appCrashes.get('')||0);
  appList.forEach(({name:n,errs,warns,crashes,cs})=>{
    row(n,n,cs,cs.reduce((t,x)=>t+x.cpu,0),cs.reduce((t,x)=>t+x.mem,0),errs,warns,crashes);
  });
}
/* Sidebar, "Containers" mode (Dozzle-style): host rows when several hosts are
   registered, then one group per app with a row per running container — status
   dot, image icon, name. The group header doubles as the app selector, so apps
   with no live container (history only) stay reachable. */
function renderContainerList(box,appList,a){
  if(S.hosts.length>1){
    const counts={};for(const x of S.conts)counts[x.host_id]=(counts[x.host_id]||0)+1;
    [['','All hosts',S.conts.length],...S.hosts.map(h=>[h,h,counts[h]||0])].forEach(([id,label,n])=>{
      const el=document.createElement('div');
      el.className='hostrow'+(S.host===id?' sel':'');
      el.dataset.host=id;el.title='Filter by host: '+label;
      el.innerHTML='<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/></svg><span class="hn"></span><span class="hsub"></span>';
      el.querySelector('.hn').textContent=label;
      el.querySelector('.hsub').textContent=n;
      el.onclick=()=>setHost(id);
      box.append(el);
    });
  }
  const gh=document.createElement('button');
  gh.className='grouphead';gh.dataset.n='';
  gh.innerHTML='<span class="chev">▾</span><span>Running</span><span class="cnt"></span>';
  gh.querySelector('.cnt').textContent=S.conts.length;
  gh.title='All apps — one merged timeline';
  gh.onclick=()=>selectApp('');
  box.append(gh);
  for(const {name,errs,warns,crashes,cs} of appList){
    const grp=document.createElement('div');grp.className='appgroup';grp.dataset.app=name;
    const head=document.createElement('div');
    head.className='apphead';head.dataset.n=name;
    head.title=name+' — open app logs ('+cs.length+' running)';
    head.innerHTML='<span class="dot"></span><span class="hn"></span><span class="badges"></span>';
    head.querySelector('.dot').classList.toggle('on',cs.length>0);
    head.querySelector('.hn').textContent=name;
    const bl=head.querySelector('.badges');
    const badge=(cls,txt,title)=>{const b=document.createElement('span');b.className=cls;b.textContent=txt;b.title=title;bl.append(b);};
    if(crashes)badge('badge e',crashes+' crash'+(crashes>1?'es':''),'crash snapshots in the last 24h');
    if(errs)badge('badge e',errs,'errors in the last hour');
    if(warns)badge('badge w',warns,'warnings in the last hour');
    head.onclick=()=>selectApp(name);
    grp.append(head);
    for(const c of cs){
      const row=document.createElement('div');
      row.className='crow';row.dataset.id=c.id;row.dataset.n=name;row.dataset.c=c.name;
      row.title=c.name+(c.image?' — '+c.image:'')+' — open logs';
      row.innerHTML='<span class="dot"></span><span class="cico"></span><span class="cn"></span>';
      row.querySelector('.cico').textContent=appIcon(c.image);
      row.querySelector('.cn').textContent=c.name;
      row.onclick=()=>{selectContainer(c.id,c);if(S.tab!=='logs')setTab('logs');};
      grp.append(row);
    }
    box.append(grp);
  }
}
/* Reflect S.app / S.selCont onto whatever sidebar mode is rendered. */
function markSidebar(){
  const appSel=n=>n===S.app||(!n&&!S.app);
  document.querySelectorAll('.app,.apphead').forEach(e=>e.classList.toggle('sel',appSel(e.dataset.n)));
  document.querySelectorAll('.grouphead').forEach(e=>e.classList.toggle('sel',!S.app));
  document.querySelectorAll('.crow[data-id]').forEach(e=>e.classList.toggle('sel',!!S.selCont&&e.dataset.id===S.selCont.id));
}
/* ---------- host selector (Phase 4.5) ---------- */
// Fetch registered hosts from the hub (agent-ingested) and local host (serve mode).
// The chip cycles through hosts; clicking it applies the host filter to
// containers, crashes, metrics and the dashboard via ?host= permalinks.
async function loadHosts(){
  try{
    const d=await api('/api/hosts');
    S.hosts=(d&&d.hosts||[]).map(h=>h.host_id).filter(Boolean);
    const hc=$('#hostchip');
    if(S.hosts.length){
      hc.style.display='';
      hc.textContent=S.host?S.host:'All hosts';
      hc.title='Filter by host: '+S.hosts.join(', ')+' (click to cycle)';
    }else{
      hc.style.display='none';
    }
  }catch(e){/* /api/hosts may not exist on older builds; hide the chip */}
}
/* Apply a host filter (chip cycle, sidebar host row, permalinks) and refresh
   every host-sensitive view. One entry point so the chip and sidebar agree. */
function setHost(h){
  if(S.host===h)return;
  S.host=h;
  $('#hostchip').textContent=h?h:'All hosts';
  syncURL();
  refreshApps();
  if(S.tab==='crashes')loadCrashes();
  if(S.tab==='dashboard' && !S.app)loadOverview();
  if(S.tab==='metrics')loadMetrics();
}
$('#hostchip').onclick=()=>{
  if(!S.hosts.length)return;
  const cur=S.hosts.indexOf(S.host);
  const nxt=(cur+1)%(S.hosts.length+1); // +1 for the "All hosts" off-state
  setHost(nxt<S.hosts.length?S.hosts[nxt]:'');
};
function fillMetricsContainers(){
  const sel=$('#mcont'),cur=sel.value;
  sel.innerHTML='<option value="">'+(S.app?'whole app':'all apps')+'</option>';
  appContainers().forEach(c=>{const o=document.createElement('option');o.value=c.id;o.textContent=(S.app?'':c.app+' / ')+'replica '+(c.replica||c.name);sel.append(o);});
  sel.value=[...sel.options].some(o=>o.value===cur)?cur:'';
}
function selectApp(n){
  S.app=n;S.sel.clear();S.selCont=null;
  markSidebar();
  fillReplicas();fillMetricsContainers();
  syncURL();
  show();
  updateContainerStat();
}
function selectContainer(id,c){
  S.app=c.app;S.sel=new Set([c.name]);S.selCont=c;
  markSidebar();
  fillReplicas();fillMetricsContainers();
  restartLogs();
  updateContainerStat();
}
function updateContainerStat(){
  const chip=$('#contStat');
  if(S.selCont){
    const cpu=Number(S.selCont.cpu||0),cpuPct=cpu.toFixed(1);
    const memBytes=Number(S.selCont.mem||0),limit=Number(S.selCont.mem_limit||0);
    const mem=limit>0?memBytes/limit*100:null,memPct=mem==null?'—':mem.toFixed(1);
    const tone=(v,warn,danger)=>v>=danger?'danger':v>=warn?'warn':'ok';
    const bar=(kind,label,pct,text,neutral=false)=>`<span class="resource-meter ${kind}" title="${label} usage"><span class="resource-label">${label} ${text}</span><span class="resource-track"><span class="resource-fill ${neutral?'neutral':tone(pct,kind==='CPU'?60:70,kind==='CPU'?80:85)}" style="width:${neutral?0:Math.min(100,Math.max(0,pct))}%"></span></span></span>`;
    chip.classList.add('resource-chip');
    chip.innerHTML=`<span class="resource-name"></span>${bar('CPU','CPU',cpu,`${cpuPct}%`)}${bar('MEM','MEM',mem||0,`${memPct}%`,mem==null)}`;
    chip.querySelector('.resource-name').textContent=S.selCont.name;
    chip.title='Live resource usage for the selected container; green is normal, amber is elevated, red is critical';
    chip.style.display='';
  }else{
    chip.classList.remove('resource-chip');
    chip.style.display='none';
  }
}

/* ---------- replica multi-select ---------- */
function appContainers(){return S.app?S.conts.filter(c=>c.app===S.app):S.conts;}
function updateReplicaBtn(){
  const n=S.sel.size,btn=$('#replicas'),total=appContainers().length;
  if(n===0)btn.textContent='All replicas ('+total+') ▾';
  else if(n===1){const c=S.conts.find(x=>[...S.sel][0]===x.name);btn.textContent='replica '+(c?(c.replica||c.name):'?')+' ▾';}
  else btn.textContent=n+' replicas ▾';
  btn.classList.toggle('on',n>0);
}
function fillReplicas(){
  for(const name of [...S.sel])if(!appContainers().some(c=>c.name===name))S.sel.delete(name);
  const pop=$('#repPop');pop.replaceChildren();
  const all=document.createElement('button');all.textContent='All replicas (clear selection)';
  all.onclick=()=>{S.sel.clear();S.selCont=null;$('#repPop').style.display='none';updateReplicaBtn();fillReplicas();restartLogs();updateContainerStat();};
  pop.append(all);
  appContainers().forEach(c=>{
    const lab=document.createElement('label');lab.className='chk';
    const cb=document.createElement('input');cb.type='checkbox';cb.checked=S.sel.has(c.name);
    cb.disabled=!cb.checked&&S.sel.size>=MAXPANES;
    cb.onchange=()=>{
      if(cb.checked)S.sel.add(c.name);else S.sel.delete(c.name);
      S.selCont=S.sel.size===1?S.conts.find(x=>x.name===[...S.sel][0]):null;
      updateReplicaBtn();fillReplicas();restartLogs();updateContainerStat();
    };
    lab.append(cb,'replica '+(c.replica||c.name));
    pop.append(lab);
  });
  const btn=$('#replicas');
  const has=appContainers().length>0;
  btn.style.display=has?'':'none';
  if(!has){$('#repPop').style.display='none';}
  updateReplicaBtn();
}
$('#replicas').onclick=e=>{e.stopPropagation();const p=$('#repPop');p.style.display=p.style.display==='none'?'':'none';};
document.addEventListener('click',e=>{if(!e.target.closest('#replicas')&&!e.target.closest('#repPop'))$('#repPop').style.display='none';});

/* ---------- tabs (v0.7: tab/app/range mirrored into the URL for permalinks) ---------- */
const TABS=['dashboard','logs','metrics','access','crashes','alerts','admin'];
function setTab(t){
  if(!TABS.includes(t)||t==='admin'&&S.role!=='admin')t='logs';
  S.tab=t;
  document.querySelectorAll('#tabs button').forEach(b=>{const on=b.dataset.t===t;b.classList.toggle('sel',on);b.setAttribute('aria-selected',on?'true':'false');});
  syncURL();
  show();
}
$('#tabs').onclick=e=>{const t=e.target.dataset.t;if(t)setTab(t);};
$('#crashchip').onclick=()=>setTab('crashes');
/* v0.7-D: collapsible sidebar (persisted) */
function setSidebar(narrow){
  S.sideNarrow=narrow;localStorage.setItem('sideNarrow',narrow?'1':'0');
  $('#main').classList.toggle('narrow',narrow);
  document.body.classList.toggle('side-narrow',narrow);
  $('#sideToggle').textContent=narrow?'⟩⟩':'⟨⟨';
  $('#sideToggle').title=narrow?'Expand sidebar':'Collapse sidebar';
}
$('#sideToggle').onclick=()=>setSidebar(!S.sideNarrow);
setSidebar(localStorage.getItem('sideNarrow')==='1');
/* v0.7-E: below 900px the sidebar becomes a drawer. #sideBtn opens it, the
   scrim or Escape closes it; body.drawer-open is inert above the breakpoint. */
function setDrawer(open){
  document.body.classList.toggle('drawer-open',open);
  const b=$('#sideBtn');if(b)b.setAttribute('aria-expanded',open?'true':'false');
}
$('#sideBtn').onclick=()=>setDrawer(!document.body.classList.contains('drawer-open'));
$('#sideScrim').onclick=()=>setDrawer(false);
document.addEventListener('keydown',e=>{if(e.key==='Escape')setDrawer(false);});
/* Sidebar mode pill: "Containers" (Dozzle-style flat list) vs "Apps" (meters). */
function syncSideMode(){
  document.querySelectorAll('#sideMode button').forEach(b=>{
    const on=b.dataset.m===S.sideMode;
    b.classList.toggle('on',on);
    b.setAttribute('aria-selected',on?'true':'false');
  });
  $('#appFilter').placeholder=S.sideMode==='containers'?'Filter containers':'Filter apps';
}
document.querySelectorAll('#sideMode button').forEach(b=>{
  b.onclick=()=>{
    if(S.sideMode===b.dataset.m)return;
    S.sideMode=b.dataset.m;localStorage.setItem('sideMode',S.sideMode);
    syncSideMode();refreshApps();
  };
});
syncSideMode();
function syncURL(){
  const p=new URLSearchParams();
  if(S.tab&&S.tab!=='dashboard')p.set('tab',S.tab);
  if(S.app)p.set('app',S.app); // empty string = All apps → omit
  if(S.host)p.set('host',S.host); // empty string = All hosts → omit
  if(S.range&&S.range!=='1h')p.set('range',S.range);
  const qs=p.toString();
  history.replaceState(null,'',location.pathname+(qs?'?'+qs:'')+location.hash);
}
/* Expiring share links (V10): a capability token in the DB that opens the
   same view until its TTL lapses; /s/<token> redirects there. The link,
   not the data, is shared — the SPA still demands a session after the hop. */
function shareQS(){
  const p=new URLSearchParams();
  if(S.tab&&S.tab!=='dashboard')p.set('tab',S.tab);
  if(S.app)p.set('app',S.app);
  if(S.host)p.set('host',S.host);
  if(S.range&&S.range!=='1h')p.set('range',S.range);
  return p.toString();
}
function currentHash(){const m=location.hash.match(/^#crash=\d+$/);return m?m[0]:'';}
async function copyShareLink(days){
  const ttl=(days||7)+'d';
  try{
    const d=await api('/api/share',{method:'POST',body:JSON.stringify({qs:shareQS(),hash:currentHash(),ttl})});
    const url=location.origin+d.url;
    try{await navigator.clipboard.writeText(url);}catch(e){prompt('Share link (expires in '+ttl+'):',url);}
    toast('Expiring share link copied — resolves for '+ttl,'ok');
  }catch(e){toast('Share link failed: '+(e.message||e),null);}
}
addEventListener('popstate',()=>{const q=new URLSearchParams(location.search);applyURL(q);});
function applyURL(q){
  const t=q.get('tab');if(t&&TABS.includes(t)){S.tab=t;document.querySelectorAll('#tabs button').forEach(b=>b.classList.toggle('sel',b.dataset.t===t));}
  const a=q.get('app');if(a!==null&&a!==S.app){S.app=a;markSidebar();fillReplicas();fillMetricsContainers();updateContainerStat();}
  const h=q.get('host');if(h!==null&&h!==S.host){S.host=h;$('#hostchip').textContent=h||'All hosts';$('#hostchip').style.display='';}
  const r=q.get('range');if(r&&(RANGE_MS[r]||r==='5m')){S.range=r;setRangeChip();}
}
function show(){
  $('#view-dashboard').style.display=(S.tab==='dashboard'&&!S.app)?'block':'none';
  $('#view-logs').style.display=S.tab==='logs'?'flex':'none';
  $('#view-metrics').style.display=S.tab==='metrics'?'flex':'none';
  $('#view-access').style.display=S.tab==='access'?'flex':'none';
  $('#view-crashes').style.display=S.tab==='crashes'?'block':'none';
  $('#view-alerts').style.display=S.tab==='alerts'?'block':'none';
  $('#view-admin').style.display=S.tab==='admin'&&S.role==='admin'?'block':'none';
  if(S.tab==='logs')restartLogs();else stopStreams();
  // Dashboard is the all-apps overview: leave it and open the selected app's
  // logs instead of rendering a per-app dashboard.
  if(S.tab==='dashboard'&&S.app)setTab('logs');
  if(S.tab==='dashboard' && !S.app)loadOverview();
  if(S.tab==='metrics')loadMetrics();
  if(S.tab==='access')loadAccess();
  if(S.tab==='crashes')loadCrashes();
  if(S.tab==='alerts')loadAlerts();
  if(S.tab==='admin'&&S.role==='admin')loadAdmin();
}

/* ---------- virtual list ---------- */
function makeVList(scroller){
  const inner=document.createElement('div');inner.className='vl-inner';scroller.append(inner);
  const win=document.createElement('div');win.className='vl-win';inner.append(win);
  const vl={rows:[],disp:[],renderRow:null,scroller,inner,win,raf:0,expanded:new Set()};
  /* v0.7-C: pattern collapse — fold runs of >=COLLAPSE_MIN consecutive
     identical (container+level+line) rows into one summary row. Click the
     summary to expand the run in place; click again to re-fold. */
  const COLLAPSE_MIN=3;
  const norm=r=>r.d?('d\0'+(r.ts||0)+'\0'+(r.version||'')):('l\0'+(r.container||'')+'\0'+(r.level||'')+'\0'+r.line);
  const runKey=g=>g.first.d?('dk\0'+g.first.ts):('k\0'+(g.first.ts||0)+'\0'+(g.first.container||'')+'\0'+g.n);
  /* Multi-line fold: lines that look like a stack trace / continuation of the
     previous entry — indents (Java, Go panics), 'at …' frames, 'Caused by:',
     Python 'Traceback', 'goroutine ', '...' elision. Heuristic, hence the
     toggle button (#group). */
  const contLine=l=>{
    if(!l)return false;
    const c=l.charAt(0);
    if(c===' '||c==='\t')return true;
    return l.startsWith('at ')||l.startsWith('Caused by:')||l.startsWith('Traceback')||l.startsWith('goroutine ')||l.startsWith('...')||l.endsWith('...');
  };
  function groupRows(rows){
    const out=[];let head=null;
    // head.m and head.cont are per-render annotations; never let them leak
    // into the next pass or attached lines would accumulate every rebuild.
    for(const r of rows){r.m=null;r.cont=false;}
    let i=0;
    while(i<rows.length){
      const r=rows[i];
      if(r.d){out.push(r);head=null;i++;continue;}
      if(S.groupLines&&head&&!head.d&&!head.c&&contLine(r.line)){
        if(vl.expanded.has('m\0'+head.id)){r.cont=true;out.push(r);}
        else{(head.m||(head.m=[])).push(r);}
        i++;continue;
      }
      let j=i+1;const k=norm(r);
      while(j<rows.length&&norm(rows[j])===k)j++;
      if(j-i>=COLLAPSE_MIN&&S.collapseDupes){
        const g={c:true,first:r,n:j-i};
        if(vl.expanded.has(runKey(g))){for(let p=i;p<j;p++)out.push(rows[p]);head=rows[j-1];}
        else{out.push(g);head=null;}
      }else{out.push(r);head=r;}
      i=j;
    }
    return out;
  }
  function rebuild(){vl.disp=groupRows(vl.rows);syncH();}
  const total=()=>vl.disp.length*ROW_H;
  function syncH(){inner.style.height=total()+'px';}
  function paint(){
    vl.raf=0;
    const st=scroller.scrollTop,h=scroller.clientHeight;
    const first=Math.max(0,Math.floor(st/ROW_H)-20);
    const last=Math.min(vl.disp.length,Math.ceil((st+h)/ROW_H)+20);
    win.style.transform='translateY('+(first*ROW_H)+'px)';
    win.replaceChildren();
    const frag=document.createDocumentFragment();
    for(let i=first;i<last;i++)frag.append(renderDisp(vl.disp[i]));
    win.append(frag);
  }
  function collapseRow(g){
    const d=document.createElement('div');d.className='ln collapse';
    const t=document.createElement('span');t.className='t';t.textContent=g.first.ts?fmtTime(g.first.ts):'';
    const c=document.createElement('span');c.className='c';c.textContent=(S.conts.find(x=>x.name===g.first.container)||{}).replica||'';
    const x=document.createElement('span');x.className='x';
    x.textContent='× '+g.n+' identical lines · '+(g.first.line||'').slice(0,120)+(g.first.line&&g.first.line.length>120?'…':'')+' — click to expand';
    x.title='Click to expand / re-fold this run of '+g.n+' repeated lines';
    d.append(t,c,x);
    d.onclick=e=>{
      e.stopPropagation();
      const k=runKey(g);
      if(vl.expanded.has(k))vl.expanded.delete(k);else vl.expanded.add(k);
      rebuild();paint();
    };
    return d;
  }
  /* one disp entry → its DOM. Groups can render to several rows (expanded
     multi-line), so this may return a DocumentFragment. */
  function renderDisp(r){
    if(r.c)return collapseRow(r);
    const el=vl.renderRow(r);
    if(r.cont)el.classList.add('cont');
    else if(r.m&&r.m.length){
      const k='m\0'+r.id;
      const b=document.createElement('span');b.className='cont-badge';
      b.textContent=(vl.expanded.has(k)?'▾ ':'▸ ')+r.m.length;
      b.title='Fold/unfold '+r.m.length+' continuation line(s) of this entry';
      b.onclick=e=>{e.stopPropagation();if(vl.expanded.has(k))vl.expanded.delete(k);else vl.expanded.add(k);rebuild();paint();};
      el.append(b);
      if(vl.expanded.has(k)){
        const f=document.createDocumentFragment();f.append(el);
        for(const cr of r.m){
          const sub=vl.renderRow(Object.assign({},cr,{cont:true}));
          sub.classList.add('cont');
          f.append(sub);
        }
        return f;
      }
    }
    return el;
  }
  vl.schedule=()=>{if(!vl.raf)vl.raf=requestAnimationFrame(paint);};
  const onScroll=()=>vl.schedule();
  scroller.addEventListener('scroll',onScroll);
  vl.destroy=()=>{
    scroller.removeEventListener('scroll',onScroll);
    if(vl.raf)cancelAnimationFrame(vl.raf);
    vl.raf=0;vl.rows=[];vl.disp=[];
  };
  vl.trim=()=>{
    if(vl.rows.length>MAXROWS){
      const cut=vl.rows.length-MAXROWS;
      const before=total();
      vl.rows.splice(0,cut);
      rebuild();
      /* collapse/grouping change the rows↔pixels ratio, so anchor by measured
         height delta instead of assuming one removed row = one removed line */
      scroller.scrollTop=Math.max(0,scroller.scrollTop-(before-total()));
    }
  };
  vl.reset=rows=>{
    vl.rows=rows.slice();vl.trim();rebuild();
    scroller.scrollTop=total();
    vl.schedule();
  };
  vl.append=(rs,pin)=>{
    if(!rs.length)return;
    const b=pin&&vl.atBottom();
    for(const r of rs)vl.rows.push(r);
    vl.trim();rebuild();
    if(b)scroller.scrollTop=total();
    vl.schedule();
  };
  vl.prepend=rs=>{
    if(!rs.length)return;
    vl.rows.unshift(...rs);vl.trim();rebuild();
    scroller.scrollTop+=rs.length*ROW_H;
    vl.schedule();
  };
  vl.atBottom=()=>scroller.scrollHeight-scroller.scrollTop-scroller.clientHeight<60;
  vl.toBottom=()=>{rebuild();scroller.scrollTop=total();};
  /* Map a raw rows[] index to its disp[] slot (collapsed runs occupy one
     slot covering n rows) and scroll there. Used by "jump to first unread",
     where the target is stored as a rows index but rows are trimmed and
     folded as new lines arrive. */
  vl.scrollToRowIndex=i=>{
    rebuild();
    let acc=0,slot=-1;
    for(let d=0;d<vl.disp.length;d++){
      const e=vl.disp[d];
      const span=e&&e.c?(e.n||1):1;
      if(acc+span>i){slot=d;break;}
      acc+=span;
    }
    if(slot<0)slot=Math.max(0,vl.disp.length-1);
    scroller.scrollTop=Math.max(0,Math.min(total(),slot*ROW_H-Math.max(0,(scroller.clientHeight-ROW_H)/2)));
    vl.schedule();
  };
  return vl;
}

/* ---------- log panes ---------- */
const sqlOn=()=>$('#smode').value==='sql';
function applyMode(){
  const on=sqlOn();
  document.body.classList.toggle('sql',on);
  $('#repPop').style.display='none';
  $('#q').placeholder=on?"SELECT ts,container,level,line FROM logs WHERE level='error' LIMIT 100":'Search logs';
  $('#q').title=on?'One SELECT over tables: logs, metrics, containers, snapshots, access':'';
}
$('#smode').addEventListener('change',applyMode);
applyMode();
function params(extra,container){
  const p=new URLSearchParams();
  if(S.app)p.set('app',S.app);
  if(container)p.set('container',container);
  if($('#q').value)p.set('q',$('#q').value);
  if($('#smode').value==='rx')p.set('regex','1');
  if($('#lvl').value)p.set('level',$('#lvl').value);
  if($('#strm').value)p.set('stream',$('#strm').value);
  if(S.range)p.set('range',S.range);
  // "Logs from container start": a per-container time floor set by the row's
  // ⏱ action. Honoured only while that container is still the selection, and
  // cleared by any filter change, so it can never leak into another view.
  if(S.logSince&&S.logSince.ts>0&&(!S.selCont||S.selCont.name===S.logSince.name))p.set('since',S.logSince.ts);
  for(const k in extra)p.set(k,extra[k]);
  return p.toString();
}
function stopStreams(){
  for(const p of S.panes){
    p.gone=true;                 // tear-down flag: reconnect closures must stop
    if(p.reconT){clearTimeout(p.reconT);p.reconT=0;}
    if(p.es){p.es.close();p.es=null;}
    if(p.destroy)p.destroy();
  }
}
function restartLogs(){
  stopStreams();
  S.panes=[];
  S.selKeys.clear();S.selData.clear();S.lastSelKey=null;S.hl.clear();updateSelBtn();
  syncLogCSV();
  if(sqlOn()){runSQL();return;}
  if(S.app===null)return;
  const sel=[...S.sel];
  if(sel.length>=2){
    $('#logs').style.display='none';$('#logs').replaceChildren();
    const box=$('#panes');box.style.display='flex';box.replaceChildren();
    sel.slice(0,MAXPANES).forEach(name=>S.panes.push(createPane(box,name,true)));
  }else{
    $('#panes').style.display='none';$('#panes').replaceChildren();
    const logs=$('#logs');logs.style.display='';
    logs.replaceChildren();
    S.panes=[createPane(logs,sel[0]||null,false)];
  }
  for(const p of S.panes)startPane(p);
  updateCnt();
}
/* SQL mode: one ad-hoc SELECT rendered into a single pane, no live stream. */
async function runSQL(){
  $('#panes').style.display='none';$('#panes').replaceChildren();
  const logs=$('#logs');logs.style.display='';logs.replaceChildren();
  const pane=createPane(logs,null,false);
  S.panes=[pane];
  const sql=$('#q').value.trim();
  updateCnt();
  if(!sql){pane.vl.reset([]);return;}
  const msg=(level,text)=>({id:1,ts:0,container:'',stream:'',level,line:text});
  pane.vl.reset([msg('','running…')]);
  try{
    const d=await api('/api/sql?query='+encodeURIComponent(sql)+'&limit=5000');
    if(S.panes[0]!==pane)return;
    pane.vl.reset(sqlRows(d));
    if(d.truncated)pane.vl.append([msg('warn','…truncated at 5000 rows — add a LIMIT or narrow the WHERE clause')],false);
    updateCnt();
    $('#conn').className='chip live';
  }catch(e){
    if(S.panes[0]!==pane)return;
    pane.vl.reset([msg('error','SQL error: '+(e.message||e))]);
    updateCnt();
  }
}
const fmtVal=v=>{
  if(v==null)return 'NULL';
  if(typeof v==='number')return Number.isInteger(v)?String(v):v.toFixed(3);
  const s=String(v);
  return s.length>600?s.slice(0,600)+'…':s; // keep blobs (logs_json, inspect) displayable
};
function sqlRows(d){
  const cols=d.columns.map(c=>c.toLowerCase()),ix=n=>cols.indexOf(n);
  const iTs=ix('ts'),iLine=ix('line'),iCont=ix('container'),iApp=ix('app'),iLvl=ix('level'),iStr=ix('stream');
  const skip=new Set([iTs,iCont,iApp,iLvl,iStr,ix('id')].filter(i=>i>=0));
  return (d.rows||[]).map((v,k)=>{
    let ts=0;
    if(iTs>=0&&v[iTs]!=null){
      const n=Number(v[iTs]);
      // logs.ts is nanoseconds, metrics/snapshots.ts are milliseconds, accept seconds too
      if(Number.isFinite(n))ts=n>1e17?Math.round(n/1e6):n>1e11?Math.round(n):n>1e9?n*1000:0;
    }
    let line;
    if(iLine>=0&&v[iLine]!=null)line=String(v[iLine]);
    else line=v.map((x,i)=>skip.has(i)?null:d.columns[i]+'='+fmtVal(x)).filter(x=>x!==null).join('  ');
    const cont=iCont>=0&&v[iCont]?String(v[iCont]):(iApp>=0&&v[iApp]?String(v[iApp]):'');
    return {id:k+1,ts,container:cont,level:iLvl>=0&&v[iLvl]?String(v[iLvl]):'',stream:iStr>=0&&v[iStr]?String(v[iStr]):'',line};
  });
}
function jumpLiveWatch(){
  const scrs=[document.querySelector('#logs'),...document.querySelectorAll('.plog')].filter(Boolean);
  const atBottom=scrs.length&&scrs.every(s=>s.scrollHeight-s.scrollTop-s.clientHeight<60);
  const jl=$('#jumpLive');
  jl.classList.toggle('show',!atBottom);
  if(atBottom)resetUnread();
  jl.classList.toggle('at-first',!!S.unreadRow&&!!S.unread);
}
/* Unread bookkeeping: rows that arrive while the view is scrolled away from
   the bottom. The pill shows the count; the first click scrolls to the oldest
   unread line, the second (or a click while already there) returns to live. */
function resetUnread(){
  S.unread=0;S.unreadRow=null;
  const jl=$('#jumpLive');
  jl.classList.remove('has-unread','at-first');
  $('#unreadN').textContent='0';
}
function noteUnread(n,firstRow){
  if(!n)return;
  const scrs=[document.querySelector('#logs'),...document.querySelectorAll('.plog')].filter(Boolean);
  const atBottom=scrs.length&&scrs.every(s=>s.scrollHeight-s.scrollTop-s.clientHeight<60);
  if(atBottom)return; // following live: nothing is unread
  // Keep the row itself, not an index: the list trims from the front as new
  // lines arrive, so a stored index would drift off the line it marked.
  if(S.unreadRow==null&&firstRow)S.unreadRow=firstRow;
  S.unread+=n;
  const jl=$('#jumpLive');
  jl.classList.add('has-unread');
  $('#unreadN').textContent=S.unread>999?'999+':String(S.unread);
  jl.title='Jump to first unread line ('+S.unread+' unread)';
}
function createPane(host,container,split){
  const pane={name:container,el:host,host,split,scroller:null,vl:null,es:null,loaded:false,pending:[],maxId:0,follow:true,paused:false};
  if(split){
    const el=document.createElement('div');el.className='pane';
    const ph=document.createElement('div');ph.className='ph';
    const c=S.conts.find(x=>x.name===container);
    const lab=document.createElement('span');lab.textContent=(c?.image?appIcon(c.image):'📦')+' replica '+(c?(c.replica||c.name):container);
    const bx=document.createElement('button');bx.className='bx';bx.textContent='✕';bx.title='Remove pane';
    bx.onclick=()=>{S.sel.delete(container);updateReplicaBtn();fillReplicas();restartLogs();};
    const pf=document.createElement('button');pf.className='bx';pf.textContent='⏸';pf.title='Pause this pane';pf.style.display=split?'':'none';
    pf.onclick=e=>{e.stopPropagation();pane.paused=!pane.paused;pf.textContent=pane.paused?'▶':'⏸';pf.title=pane.paused?'Resume this pane':'Pause this pane';if(!pane.paused)flushPending(pane);};
    const fw=document.createElement('button');fw.className='bx';fw.textContent=pane.follow?'👁':'🙈';fw.title='Follow this pane';fw.style.display=split?'':'none';
    fw.onclick=e=>{e.stopPropagation();pane.follow=!pane.follow;fw.textContent=pane.follow?'👁':'🙈';if(pane.follow)pane.vl.toBottom();};
    ph.append(lab,fw,pf,bx);el.append(ph);
    const scroller=document.createElement('div');scroller.className='plog';
    el.append(scroller);
    host.append(el);
    pane.el=el;pane.scroller=scroller;
  }else{
    pane.scroller=host; // #logs itself is the scroll container
  }
  const vl=makeVList(pane.scroller);
  vl.renderRow=lineEl;
  pane.vl=vl;
  // v0.7 fix: fw (follow button) only exists in split panes; look it up per-pane
  // instead of referencing the out-of-scope closure variable (ReferenceError).
  const followBtn=split?pane.el.querySelector('.ph .bx:nth-child(2)'):null;
  const followOff=()=>{
    if(!vl.atBottom()&&pane.follow){pane.follow=false;if(followBtn)followBtn.textContent='🙈';}
    jumpLiveWatch();
  };
  pane.scroller.addEventListener('scroll',followOff);
  jumpLiveWatch();
  $('#jumpLive').onclick=()=>{
    const jl=$('#jumpLive');
    // First click: jump to the oldest unread line. Second click (or a click
    // when there is nothing unread): return to live tail.
    if(S.unread&&S.unreadRow&&!jl.classList.contains('at-first')){
      // Locate the stored row among current rows; if it was trimmed away,
      // fall back to the oldest row still present.
      const p=S.panes.find(p=>p.vl&&p.vl.rows.length);
      if(p){
        let idx=p.vl.rows.indexOf(S.unreadRow);
        if(idx<0)idx=0;
        p.vl.scrollToRowIndex(idx);
        jl.classList.add('at-first');
        jl.title='Back to live logs';
        return;
      }
    }
    document.querySelectorAll('#logs,.plog').forEach(s=>{s.scrollTop=s.scrollHeight;});
    resetUnread();
    jl.classList.remove('show','at-first');
  };
  pane.destroy=()=>{pane.scroller.removeEventListener('scroll',followOff);vl.destroy();};
  return pane;
}
/* ---- live-tail connection: open, resume, reconnect ----
   The server tags every SSE row with `id:`. When EventSource reconnects (its
   native retry or our backoff loop below) it sends Last-Event-ID, and the
   server only forwards hub rows newer than that — no duplicates, no gap while
   the connection was down. For long outages we additionally replay the missed
   window from SQLite (catchUp) since the hub has no replay buffer. */
function openStream(pane,first){
  // Require a valid session before opening the live-tail stream.
  // Without one, EventSource gets 401 and retries forever silently.
  // (When the server runs with auth disabled there is no token to hold —
  // don't bounce the user to a login form they can never complete.)
  if(!localStorage.getItem('capscope_token')&&S.authEnabled){
    if(!pane.gone)showLogin();
    return;
  }
  const my=pane.es=new EventSource('/api/logs/stream?'+params(pane.maxId>0?{since_id:pane.maxId}:{},pane.name));
  my.onopen=()=>{
    if(pane.gone)return;
    connChip('live','live');pane.retries=0;
    // Any reconnect after the initial backfill (our backoff loop or the
    // browser's native retry): replay rows published while disconnected.
    if(pane.loaded)catchUp(pane);
  };
  my.onmessage=e=>{
    if(pane.gone)return;
    let r;try{r=JSON.parse(e.data);}catch(err){return;}
    // pane.maxId is the delivery cursor: advanced only by paneAppend when a row
    // is actually delivered (keeps paused/resume dedup correct). Do NOT advance
    // it here — onmessage runs before paneAppend, so advancing here would make
    // paneAppend's r.id > pane.maxId check drop every live row.
    if(!pane.loaded||pane.paused)pane.pending.push(r);
    else paneAppend(pane,[r],true);
  };
  my.onerror=()=>{
    if(pane.gone)return;
    // If we lost our session, stop retrying and show the login overlay.
    if(!localStorage.getItem('capscope_token')&&S.authEnabled){
      if(my.readyState!==EventSource.CLOSED)my.close();
      pane.es=null;
      showLogin();
      return;
    }
    if(my.readyState===EventSource.CLOSED){ // EventSource gave up — take over
      pane.es=null;
      scheduleReconnect(pane);
    }else connChip('bad','reconnecting…'); // native retry will resume via Last-Event-ID
  };
  // Server emits "event: dropped" (cumulative count) on its 15s ping when the
  // hub had to discard lines for this subscriber under write pressure.
  my.addEventListener('dropped',e=>{const n=e.data|0;if(n){connChip('live','live ('+n+' dropped)');$('#conn').title=n+' live line(s) dropped by the SSE buffer (write pressure). Persisted logs, crash snapshots and CSV export are unaffected.';}});
}
function scheduleReconnect(pane){
  pane.retries=(pane.retries||0)+1;
  const wait=Math.min(15000,1000*Math.pow(2,Math.min(pane.retries-1,4)))+Math.floor(Math.random()*750);
  connChip('bad','reconnecting (attempt '+pane.retries+')');
  pane.reconT=setTimeout(()=>{if(!pane.gone)openStream(pane,false);},wait);
}
/* replay rows published between the last delivered id and now, from SQLite */
async function catchUp(pane){
  if(!(pane.maxId>0)||!pane.loaded)return;
  try{
    const d=await api('/api/logs?'+params({since_id:pane.maxId,asc:'1',limit:2000},pane.name));
    if(pane.gone||!d.logs||!d.logs.length)return;
    pane.pending.push(...d.logs);
    if(pane.paused)return; // stay paused: rows flush on Resume
    flushPending(pane);
  }catch(e){/* transient: the next reconnect retries */}
}
async function startPane(pane){
  pane.loaded=false;pane.pending=[];pane.maxId=0;pane.retries=0;pane.gone=false;
  openStream(pane,true);
  // "From start" opens the container's oldest retained lines and streams the
  // history forward, so the view anchors on boot instead of the tail.
  const fromStart=!!(S.logSince&&S.logSince.fromStart&&S.logSince.name===pane.name);
  if(fromStart)S.logSince.fromStart=false; // one-shot: a reload must not redo it
  try{
    const d=await api('/api/logs?'+params(fromStart?{limit:500,asc:'1'}:{limit:500},pane.name));
    if(pane.gone)return;
    let maxId=0;for(const r of d.logs)if(r.id>maxId)maxId=r.id;
    pane.maxId=Math.max(pane.maxId,maxId); // live rows may have arrived during load
    // V9: interleave deploy markers that fall inside the loaded window.
    const oldest=d.logs.length?d.logs[0].ts:Date.now()-3600e3;
    const deploys=await loadDeploys(null,oldest);
    pane.vl.reset(mergeDeploys(d.logs,deploys));
    if(fromStart&&d.logs.length>=500)await backfillHistory(pane);
    pane.loaded=true;
    flushPending(pane);
    // Anchor on the first line when opened from the start; otherwise follow live.
    if(fromStart)pane.vl.scrollToRowIndex(0);else pane.vl.toBottom();
  }catch(e){connBad('load failed');}
}
/* Walk the retained history forward from pane.maxId in bounded chunks.
   pane.loaded stays false while this runs, so live rows queue in pane.pending
   and are flushed afterwards in order — no interleaving, and the unread pill
   is not spammed by our own backfill. */
const BACKFILL_CHUNKS=20; // ×2000 rows: bounds the work on a very chatty container
async function backfillHistory(pane){
  for(let i=0;i<BACKFILL_CHUNKS;i++){
    if(pane.gone)return;
    let d;
    try{d=await api('/api/logs?'+params({since_id:pane.maxId,asc:'1',limit:2000},pane.name));}
    catch(e){toast('History load interrupted — the rest is still there','err');return;}
    if(!d.logs||!d.logs.length)break;
    for(const r of d.logs)if(r.id>pane.maxId)pane.maxId=r.id;
    pane.vl.append(d.logs,false); // pin=false: keep the view anchored at the top
    if(d.logs.length<2000){updateCnt();return;} // caught up
  }
  updateCnt();
  toast('Loaded the first '+(BACKFILL_CHUNKS*2000)+' lines from the start — newer ones keep streaming in','ok');
}
function paneAppend(pane,rs,pin){
  const fresh=[];
  for(const r of rs){if(r.d||r.id>pane.maxId){if(!r.d)pane.maxId=r.id;fresh.push(r);}}
  if(fresh.length){
    const wasBottom=pane.vl.atBottom();
    pane.vl.append(fresh,pin&&pane.follow&&wasBottom);
    if(!wasBottom)noteUnread(fresh.length,fresh[0]);
  }
  updateCnt();
}
function flushPending(pane){
  const p=pane.pending;pane.pending=[];
  if(p.length)paneAppend(pane,p,true);
  updateCnt();
}
function updateCnt(){
  let n=0;for(const p of S.panes)n+=p.vl.rows.length;
  $('#cnt').textContent=n+(sqlOn()?' rows':' lines');
  renderLegend();
}
/* Level legend: live per-level counts for the rows currently loaded. A chip
   filters to that level (click again to clear); the active chip mirrors #lvl.
   Counts reflect what is loaded, so they shrink while a level filter is on. */
function renderLegend(){
  const box=$('#legend');if(!box)return;
  const cnt={error:0,warn:0,info:0,debug:0};
  for(const p of S.panes){
    const rows=p.vl&&p.vl.rows;if(!rows)continue;
    for(const r of rows){if(!r.d&&cnt[r.level]!=null)cnt[r.level]++;}
  }
  const cur=$('#lvl').value;
  const active=new Set(cur?cur.split(','):[]);
  box.replaceChildren();
  for(const lvl of ['error','warn','info','debug']){
    const b=document.createElement('button');b.type='button';b.className='lg '+lvl;
    const dot=document.createElement('span');dot.className='d';
    const lb=document.createElement('span');lb.textContent=lvl;
    const n=document.createElement('b');n.textContent=cnt[lvl];
    b.append(dot,lb,n);
    b.title='Show only '+lvl+' lines ('+cnt[lvl]+' loaded) — click again to clear';
    b.classList.toggle('on',active.has(lvl));
    b.setAttribute('aria-pressed',active.has(lvl)?'true':'false');
    b.onclick=()=>{
      const sel=$('#lvl');
      sel.value=sel.value===lvl?'':lvl;
      renderLegend();
      redo();
    };
    box.append(b);
  }
}
$('#follow').onclick=()=>{const nf=!S.panes.some(p=>p.follow);for(const p of S.panes){p.follow=nf;const fw=p.el.querySelector('.bx:nth-child(2)');if(fw)fw.textContent=nf?'👁':'🙈';if(nf)p.vl.toBottom();}$('#follow').classList.toggle('on',nf);};
$('#wrap').onclick=()=>{S.wrapLines=!S.wrapLines;$('#wrap').classList.toggle('on',S.wrapLines);document.documentElement.classList.toggle('wrap',S.wrapLines);};
$('#group').classList.toggle('on',S.groupLines);
$('#group').onclick=()=>{S.groupLines=!S.groupLines;localStorage.setItem('groupLines',S.groupLines?'1':'0');$('#group').classList.toggle('on',S.groupLines);toast('Stack-trace grouping '+(S.groupLines?'on':'off'));for(const p of S.panes)if(p.vl&&p.vl.rows)p.vl.reset(p.vl.rows);};
function applyAppFilter(){
  const v=$('#appFilter').value.toLowerCase();
  const box=$('#apps');
  // Apps mode: plain per-app rows match on the app name.
  box.querySelectorAll('.app').forEach(e=>{e.style.display=e.dataset.n.toLowerCase().includes(v)?'':'none';});
  // Containers mode: match on app name or container name; hide groups left empty.
  box.querySelectorAll('.appgroup').forEach(g=>{
    let shown=0;
    g.querySelectorAll('.crow').forEach(r=>{
      const hit=!v||r.dataset.n.toLowerCase().includes(v)||String(r.dataset.c||'').toLowerCase().includes(v);
      r.style.display=hit?'':'none';
      if(hit)shown++;
    });
    g.style.display=(!v||shown||g.dataset.app.toLowerCase().includes(v))?'':'none';
  });
  box.querySelectorAll('.hostrow').forEach(e=>{e.style.display=(!v||e.textContent.toLowerCase().includes(v))?'':'none';});
  // The "Running" summary header is meaningless while a filter is active.
  box.querySelectorAll('.grouphead').forEach(e=>{e.style.display=v?'none':'';});
}
$('#appFilter').addEventListener('input',applyAppFilter);
$('#appFilter').addEventListener('keydown',e=>{
  if(e.key==='Escape'){e.stopPropagation();$('#appFilter').value='';applyAppFilter();e.target.blur();}
});
$('#pause').onclick=()=>{const np=!S.panes.some(p=>p.paused);for(const p of S.panes){p.paused=np;const pf=p.el.querySelector('.bx:nth-child(3)');if(pf)pf.textContent=np?'⏸':'▶';if(!np)flushPending(p);}$('#pause').classList.toggle('on',np);$('#pause').textContent=np?'Pause':'Resume';};
function setRangeChip(){document.querySelectorAll('#r5m,#r15m,#r1h,#r24h').forEach(b=>b.classList.toggle('on',b.dataset.range===S.range));}
function setStatusChip(){document.querySelectorAll('#aall,#a2xx,#a4xx,#a5xx').forEach(b=>b.classList.toggle('on',b.dataset.status===S.accStatus));}
setRangeChip();setStatusChip();
document.querySelectorAll('#r5m,#r15m,#r1h,#r24h').forEach(b=>b.onclick=()=>{S.logSince=null;S.range=b.dataset.range;setRangeChip();syncLogCSV();syncURL();restartLogs();});
document.querySelectorAll('#aall,#a2xx,#a4xx,#a5xx').forEach(b=>b.onclick=()=>{S.accStatus=b.dataset.status;setStatusChip();loadAccess();});
$('#jsonBtn').onclick=()=>{S.jsonPretty=!S.jsonPretty;$('#jsonBtn').classList.toggle('on',S.jsonPretty);restartLogs();};
$('#older').onclick=()=>{for(const p of S.panes)loadOlder(p);};
async function loadOlder(pane){
  const rows=pane.vl.rows;
  if(!rows.length)return;
  try{
    // rows[0] may be a deploy marker (no id); find the first real log row.
    let beforeId=0;for(const r of rows){if(!r.d){beforeId=r.id;break;}}
    if(!beforeId){updateCnt();return;}
    const oldestMs=rows[0].ts;
    const d=await api('/api/logs?'+params({limit:500,before_id:beforeId},pane.name));
    if(d.logs.length){
      const deploys=await loadDeploys(null,d.logs[0].ts);
      // keep only markers older than what is already shown
      const shown=new Set(rows.filter(r=>r.d).map(r=>r.ts));
      const older=deploys.filter(dp=>dp.ts<oldestMs&&!shown.has(dp.ts));
      pane.vl.prepend(mergeDeploys(d.logs,older));
    }
    updateCnt();
  }catch(e){connBad('load older failed');console.warn('capscope: load older failed',e);}
}
let deb;const redo=()=>{clearTimeout(deb);deb=setTimeout(restartLogs,300);};
['#q','#smode','#lvl','#strm'].forEach(s=>$(s).addEventListener(s==='#q'?'input':'change',()=>{S.logSince=null;redo();if(s==='#lvl')renderLegend();}));

/* ---------- row rendering ---------- */
function highlight(el,text){
  const q=$('#q').value;
  if(!q||$('#smode').value!==''){el.textContent=text;return;}
  const lo=text.toLowerCase(),ql=q.toLowerCase();let i=0,j;
  while((j=lo.indexOf(ql,i))>=0){el.append(text.slice(i,j));const m=document.createElement('mark');m.textContent=text.slice(j,j+q.length);el.append(m);i=j+q.length;}
  el.append(text.slice(i));
}
/* JSON detection: cheap prefix check, then one parse (only for visible rows). */
function asJSON(line){
  const s=line.trim();
  if(s.length<2||s[0]!=='{'||s.length>200000)return null;
  try{const o=JSON.parse(s);return (o&&typeof o==='object')?o:null;}catch(e){return null;}
}
/* V8: pretty-print one JSON log line in an overlay (rows stay single-line). */
function showJSON(line){
  const o=asJSON(line);
  if(!o)return;
  const w=document.createElement('div');w.className='modal';
  const box=document.createElement('div');box.className='box';
  const mh=document.createElement('div');mh.className='mh';
  const t=document.createElement('span');t.textContent='JSON log line';
  const x=document.createElement('button');x.className='b';x.textContent='Close';
  mh.append(t,x);
  const pre=document.createElement('pre');pre.textContent=JSON.stringify(o,null,2);
  box.append(mh,pre);w.append(box);
  const close=()=>{w.remove();document.removeEventListener('keydown',esc);};
  const esc=e=>{if(e.key==='Escape')close();};
  x.onclick=close;
  w.onclick=e=>{if(e.target===w)close();};
  document.addEventListener('keydown',esc);
  document.body.append(w);
  x.focus();
}
function lineEl(r){
  // V9: a deploy marker row rendered inline in the timeline.
  if(r.d){
    const m=document.createElement('div');m.className='ln deploy';
    const t=document.createElement('span');t.className='t';t.textContent=r.ts?fmtTime(r.ts):'';
    const c=document.createElement('span');c.className='c';c.textContent='▲';
    const x=document.createElement('span');x.className='x';
    x.textContent='deploy '+(r.version||'')+(r.note?' — '+r.note:'');
    x.title=(r.version||'')+(r.note?'\n'+r.note:'')+'\nrecorded '+fmtDT(r.ts);
    m.append(t,c,x);return m;
  }
  const d=document.createElement('div');d.className='ln '+(r.level||'')+(r.stream==='e'?' e':'');d.dataset.key=lineKey(r);d.dataset.ts=r.ts||'';d.dataset.container=r.container||'';d.dataset.line=r.line||'';
  if(S.selKeys.has(d.dataset.key))d.classList.add('sel');
  if(S.hl.has(d.dataset.key))d.classList.add('hl');
  const t=document.createElement('span');t.className='t lts';t.textContent=r.ts?fmtTime(r.ts):'';
  // Click the timestamp to copy the full date+time (the mono column only shows HH:MM:SS).
  t.title=r.ts?'Click to copy '+fmtDT(r.ts):'';
  t.onclick=e=>{e.stopPropagation();if(r.ts)copyText(fmtDT(r.ts),'Timestamp copied');};
  const c=document.createElement('span');c.className='c';c.textContent=(S.conts.find(x=>x.name===r.container)||{}).replica||r.container.split('.')[1]||r.container||'';
  const x=document.createElement('span');x.className='x';x.title=r.line;highlight(x,r.line);
  const o=asJSON(r.line);
  if(o){
    d.classList.add('json');
    x.title='JSON log line — click to pretty-print';
    x.onclick=()=>showJSON(r.line);
    if(S.jsonPretty){
      d.classList.add('json-pretty');
      const pre=document.createElement('pre');pre.textContent=JSON.stringify(o,null,2);
      d.append(pre);
    }
  }
  d.append(t,c,x);
  // Hover actions: copy, filter to this container, filter to this level.
  const mkAct=(glyph,title,fn)=>{
    const b=document.createElement('button');b.type='button';b.className='act';
    b.textContent=glyph;b.title=title;
    b.onclick=e=>{e.stopPropagation();fn();};
    return b;
  };
  const lacts=document.createElement('span');lacts.className='lacts';
  lacts.append(mkAct('⧉','Copy line',()=>copyText(r.line,'Line copied')));
  lacts.append(mkAct('⌗','Show only this container',()=>{
    const tc=S.conts.find(z=>z.name===r.container);
    if(!tc){toast('Container '+r.container+' is not tracked','err');return;}
    selectContainer(tc.id,tc);
    if(S.tab!=='logs')setTab('logs');
    toast('Filtered to '+r.container);
  }));
  if(r.level){
    lacts.append(mkAct('▼','Show only '+r.level+' lines',()=>{
      const sel=$('#lvl');
      if(sel.value===r.level){sel.value='';toast('Level filter cleared');}
      else{sel.value=r.level;toast('Level filter: '+r.level);}
      renderLegend();redo();
    }));
  }
  d.append(lacts);
  d.onclick=e=>selToggle(r,e);
  d.ondblclick=e=>{e.stopPropagation();const k=d.dataset.key;if(S.hl.has(k))S.hl.delete(k);else{S.hl.add(k);S.selData.set(k,{ts:r.ts,container:r.container,line:r.line});}renderSel();};
  // v0.7-C: context menu — open ±N lines around this row in the context modal.
  d.oncontextmenu=e=>{e.preventDefault();e.stopPropagation();openCtx(r);};
  return d;
}

/* ---------- v0.7-C: log context modal (±50 lines around a row) ---------- */
async function openCtx(r){
  const w=document.createElement('div');w.className='modal';
  const box=document.createElement('div');box.className='box';box.style.maxWidth='980px';box.style.width='92vw';
  const mh=document.createElement('div');mh.className='mh';
  const t=document.createElement('span');t.textContent='Context · '+(r.container||'all')+' @ '+fmtTime(r.ts);
  const x=document.createElement('button');x.className='b';x.textContent='Close';
  mh.append(t,x);
  const info=document.createElement('pre');info.textContent='loading…';
  box.append(mh,info);w.append(box);document.body.append(w);
  const close=()=>{w.remove();document.removeEventListener('keydown',esc);};
  const esc=e=>{if(e.key==='Escape')close();};
  x.onclick=close;w.onclick=e=>{if(e.target===w)close();};
  document.addEventListener('keydown',esc);
  try{
    const p=params({limit:100,before_id:r.id},r.container);
    const [d,after]=await Promise.all([
      api('/api/logs?'+p),
      r.id?api('/api/logs?'+params({limit:50,since_id:r.id+1,asc:'1'},r.container)):Promise.resolve({logs:[]})
    ]);
    const rows=(d.logs||[]).concat(after.logs||[]);
    if(!rows.length){info.textContent='(no lines found around this row)';return;}
    const W=50;let anchor=rows.findIndex(q=>q.id===r.id);
    if(anchor<0)anchor=rows.length-1;
    const lo=Math.max(0,anchor-W),hi=Math.min(rows.length,anchor+W+1);
    info.replaceChildren();
    for(let i=lo;i<hi;i++){
      const q=rows[i];
      const el=lineEl(q);
      el.style.height='auto';el.style.whiteSpace='pre-wrap';
      if(q.id===r.id){el.classList.add('hl');el.title='selected line';}
      info.append(el);
    }
    info.style.maxHeight='60vh';info.style.overflow='auto';info.style.font='12px/20px var(--font-mono)';
    const selEl=info.children[Math.min(anchor-lo,W)];
    if(selEl)selEl.scrollIntoView({block:'center'});
  }catch(e){info.textContent='context load failed: '+(e.message||e);}
}

/* ---------- line selection (export) ---------- */
function lineKey(r){return`${r.ts||0}\0${r.container||''}\0${r.line}`;}
function renderSel(){
  document.querySelectorAll('#logs .ln,.plog .ln').forEach(d=>{
    const k=d.dataset.key||'';
    d.classList.toggle('sel',S.selKeys.has(k));
    d.classList.toggle('hl',S.hl.has(k));
  });
  updateSelBtn();
}
function updateSelBtn(){
  const n=S.selKeys.size,btn=$('#exportSel');
  if(!btn)return;
  if(n===0){btn.style.display='none';return;}
  btn.style.display='';btn.textContent='Export ('+n+') lines';
}
function selToggle(r,e){
  const key=lineKey(r);
  if(e.shiftKey&&S.lastSelKey){S.selKeys.add(key);}else{S.selKeys.clear();S.selKeys.add(key);}
  S.selData.set(key,{ts:r.ts,container:r.container,line:r.line});
  S.lastSelKey=key;
  renderSel();
}

/* ---------- access dashboard (GoAccess-style) ---------- */
const fmtNum=n=>(n||0).toLocaleString();
async function loadAccess(){
  const range=$('#arange').value;let d;
  try{d=await api('/api/access?range='+range+(S.accStatus?'&status='+S.accStatus:''));}catch(e){connBad('access load failed');console.warn('capscope: access load failed',e);return;}
  $('#accCSV').href='/api/access/export?range='+range+(S.accStatus?'&status='+S.accStatus:'');
  renderTotals($('#atotals'),d.totals||{});
  drawAccessTimeline($('#atl'),d.timeline||[]);
  fillCodes($('#acodes'),d.codes||[],(d.totals||{}).requests);
  fillTop($('#apaths'),d.paths||[]);
  fillTop($('#aips'),d.ips||[]);
  fillTop($('#arefs'),d.refs||[]);
}
function renderTotals(host,t){
  const errs=(t.client_err||0)+(t.server_err||0);
  host.replaceChildren();
  [['Requests',fmtNum(t.requests)],['4xx errors',fmtNum(t.client_err)],['5xx errors',fmtNum(t.server_err)],['Unique client IPs',fmtNum(t.unique_ips)],['Traffic',mib(t.bytes)]].forEach(([k,v])=>{
    const a=document.createElement('b');a.textContent=k;const b=document.createElement('span');b.textContent=v;host.append(a,b);
  });
  if(!(t.requests||errs)){const e=document.createElement('div');e.style.color='var(--muted)';e.textContent='No access-log rows in this window yet. CapScope switches a container to access-log mode automatically once its stdout looks like web-server logs (or set ACCESS_LOG=all); excluded containers like the reverse proxy are followed only for this.';host.append(e);}
}
function fillCodes(host,codes,total){
  host.replaceChildren();
  if(!codes.length){host.innerHTML='<tr><td style="color:var(--muted)">no data</td></tr>';return;}
  const tr=document.createElement('tr');['Code','Requests',''].forEach(h=>{const th=document.createElement('th');th.textContent=h;tr.append(th);});
  host.append(tr);
  for(const c of codes){
    const tr=document.createElement('tr');
    const n=c.n.toLocaleString();
    const pct=total?Math.round(100*c.n/total):0;
    tr.innerHTML='<td>'+c.code+'</td><td style="font-variant-numeric:tabular-nums">'+n+'</td>';
    const td=document.createElement('td');td.innerHTML='<span class="k '+(c.code>=500?'oom':c.code>=400?'crash':'clean_exit')+'">'+pct+'%</span>';
    tr.append(td);host.append(tr);
  }
}
function fillTop(host,rows){
  host.replaceChildren();
  if(!rows.length){host.innerHTML='<tr><td style="color:var(--muted)">no data</td></tr>';return;}
  const tr=document.createElement('tr');['Value','Hits','Errors'].forEach(h=>{const th=document.createElement('th');th.textContent=h;tr.append(th);});
  host.append(tr);
  for(const r of rows.slice(0,20)){
    const tr=document.createElement('tr');tr.className='row';
    const td=document.createElement('td');td.textContent=r.key;td.style.maxWidth='260px';td.title=r.key;
    const n=document.createElement('td');n.textContent=fmtNum(r.hits);n.style.fontVariantNumeric='tabular-nums';
    const e=document.createElement('td');
    if(r.errors){e.textContent=fmtNum(r.errors);e.style.color='var(--err)';}
    tr.append(td,n,e);host.append(tr);
  }
}
function drawAccessTimeline(canvas,pts){
  const dpr=devicePixelRatio||1,w=canvas.clientWidth,h=canvas.clientHeight;
  canvas.width=w*dpr;canvas.height=h*dpr;const g=canvas.getContext('2d');g.scale(dpr,dpr);
  const css=getComputedStyle(document.documentElement);const col=n=>css.getPropertyValue(n).trim();
  g.clearRect(0,0,w,h);
  if(!pts.length){g.fillStyle=col('--muted');g.font='13px system-ui';g.fillText('No requests in this range yet',12,24);return;}
  const L=52,R=8,T=8,B=20,x0=pts[0].ts,x1=pts[pts.length-1].ts||x0+1;
  const max=Math.max(1,...pts.map(p=>Math.max(p.hits,p.errors)))*1.15;
  const X=t=>L+(t-x0)/(x1-x0||1)*(w-L-R),Y=v=>T+(1-v/max)*(h-T-B);
  g.strokeStyle=col('--line');g.fillStyle=col('--muted');g.font='11px system-ui';g.lineWidth=1;
  for(let i=0;i<=4;i++){const v=max*i/4,y=Y(v);g.beginPath();g.moveTo(L,y);g.lineTo(w-R,y);g.stroke();g.fillText(String(Math.round(v)),4,y+4);}
  g.fillText(fmtTime(x0).slice(0,5),L,h-4);const e=fmtTime(x1).slice(0,5);g.fillText(e,w-R-g.measureText(e).width,h-4);
  const line=(key,color)=>{
    g.strokeStyle=color;g.lineWidth=1.6;g.beginPath();
    pts.forEach((p,i)=>{const x=X(p.ts),y=Y(p[key]);i?g.lineTo(x,y):g.moveTo(x,y);});g.stroke();
  };
  line('hits',col('--accent'));
  line('errors',col('--err'));
}
function syncAccCSV(){$('#accCSV').href='/api/access/export?range='+$('#arange').value;}
$('#arange').onchange=()=>{syncAccCSV();loadAccess();};
syncAccCSV();

/* ---------- metrics ---------- */
function draw(canvas,pts,key,fmt,limitKey,deploys,crashes,lineColor,label){
  if(!lineColor)lineColor='--accent';
  // Remember how this chart was drawn so the shared crosshair, the tooltip
  // and a refresh can redraw it without re-plumbing the arguments.
  canvas._cfg={pts,key,fmt,limitKey,deploys,crashes,lineColor,label};
  const dpr=devicePixelRatio||1,w=canvas.clientWidth,h=canvas.clientHeight;
  canvas.width=w*dpr;canvas.height=h*dpr;const g=canvas.getContext('2d');g.scale(dpr,dpr);
  const css=getComputedStyle(document.documentElement);const col=n=>css.getPropertyValue(n).trim();
  g.clearRect(0,0,w,h);
  if(!pts.length){g.fillStyle=col('--muted');g.font='13px system-ui';g.fillText('No samples in this range yet',12,24);return;}
  const L=52,R=8,T=8,B=20;
  let x0=pts[0].ts,x1=pts[pts.length-1].ts||x0+1,series=pts;
  // Drag-zoom: clip to the selected window. Snap-back (dblclick / the reset
  // chip) clears S.metricZoom and the whole range comes back.
  if(S.metricZoom){
    const z=S.metricZoom;
    if(z.t0>x0)x0=z.t0;
    if(z.t1<x1)x1=z.t1;
    if(x1<=x0){x0=pts[0].ts;x1=pts[pts.length-1].ts||x0+1;series=pts;}
    else series=pts.filter(p=>p.ts>=x0&&p.ts<=x1);
  }
  // px↔time mapping for the pointer handlers (offsets let the drag box live
  // in card coordinates while the maths stays in canvas coordinates).
  canvas._dom={x0,x1,L,R,w,ox:canvas.offsetLeft,oy:canvas.offsetTop,h};
  if(!series.length){g.fillStyle=col('--muted');g.font='13px system-ui';g.fillText('No samples in this window',12,24);return;}
  let max=Math.max(...series.map(p=>p[key]));const lim=limitKey?pts[pts.length-1][limitKey]:0;
  if(lim>0&&lim<max*4)max=Math.max(max,lim);max=max*1.1||1;
  const X=t=>L+(t-x0)/(x1-x0||1)*(w-L-R),Y=v=>T+(1-v/max)*(h-T-B);
  // Live drag selection (only on the chart being dragged).
  if(S.dragSel&&S.dragSel.cv===canvas.id){
    const a=Math.min(S.dragSel.a,S.dragSel.b),b=Math.max(S.dragSel.a,S.dragSel.b);
    if(b-a>2){
      g.save();g.globalAlpha=.16;g.fillStyle=col('--accent');g.fillRect(a,T,b-a,h-T-B);g.restore();
      g.strokeStyle=col('--accent');g.lineWidth=1;
      g.beginPath();g.moveTo(a,T);g.lineTo(a,h-B);g.moveTo(b,T);g.lineTo(b,h-B);g.stroke();
    }
  }
  g.strokeStyle=col('--line');g.fillStyle=col('--muted');g.font='11px system-ui';g.lineWidth=1;
  for(let i=0;i<=4;i++){const v=max*i/4,y=Y(v);g.beginPath();g.moveTo(L,y);g.lineTo(w-R,y);g.stroke();g.fillText(fmt(v),4,y+4);}
  g.fillText(fmtTime(x0).slice(0,5),L,h-4);const e=fmtTime(x1).slice(0,5);g.fillText(e,w-R-g.measureText(e).width,h-4);
  if(lim>0){g.strokeStyle=col('--err');g.setLineDash([5,4]);g.beginPath();g.moveTo(L,Y(lim));g.lineTo(w-R,Y(lim));g.stroke();g.setLineDash([]);}
  // V9: deploy markers — a dashed vertical line with a ▲ flag and version label.
  if(deploys&&deploys.length){
    g.save();g.font='10px system-ui';
    for(const d of deploys){
      if(d.ts<x0||d.ts>x1)continue;
      const x=X(d.ts);
      g.strokeStyle=col('--accent');g.setLineDash([3,3]);g.lineWidth=1;
      g.beginPath();g.moveTo(x,T);g.lineTo(x,h-B);g.stroke();g.setLineDash([]);
      g.fillStyle=col('--accent');g.beginPath();g.moveTo(x,T);g.lineTo(x-4,T-0);g.lineTo(x+4,T);g.closePath();g.fill();
      const lab=d.version||d.app;const tw=g.measureText(lab).width;
      let lx=x+4;if(lx+tw>w-R)lx=x-4-tw;
      g.fillText(lab,lx,T+9);
    }
    g.restore();
  }
  // Crash markers — red dashed vertical line with a ✕ flag.
  if(crashes&&crashes.length){
    g.save();g.font='10px system-ui';
    for(const c of crashes){
      if(c.ts<x0||c.ts>x1)continue;
      const x=X(c.ts);
      g.strokeStyle=col('--err');g.setLineDash([2,2]);g.lineWidth=1;
      g.beginPath();g.moveTo(x,T);g.lineTo(x,h-B);g.stroke();g.setLineDash([]);
      g.fillStyle=col('--err');g.beginPath();g.moveTo(x-3,T);g.lineTo(x+3,T+6);g.lineTo(x,T+3);g.closePath();g.fill();
      const lab=c.kind?c.kind.replace('_',' '):'crash';const tw=g.measureText(lab).width;
      let lx=x+4;if(lx+tw>w-R)lx=x-4-tw;
      g.fillStyle=col('--err');g.fillText(lab,lx,T+9);
    }
    g.restore();
  }
  g.strokeStyle=col(lineColor);g.lineWidth=1.6;g.beginPath();
  series.forEach((p,i)=>{const x=X(p.ts),y=Y(p[key]);i?g.lineTo(x,y):g.moveTo(x,y);});g.stroke();
  // Shared crosshair: the hovered index is drawn on BOTH charts so they stay
  // in step, with the dot marking the value behind the tooltip.
  if(S.crossIdx!=null&&pts.length){
    const i=Math.max(0,Math.min(pts.length-1,S.crossIdx));
    const p=pts[i];
    if(p.ts>=x0&&p.ts<=x1&&isFinite(Number(p[key]))){
      const cx=X(p.ts);
      g.save();
      g.strokeStyle=col('--accent');g.setLineDash([3,3]);g.lineWidth=1;
      g.beginPath();g.moveTo(cx,T);g.lineTo(cx,h-B);g.stroke();
      g.setLineDash([]);
      g.fillStyle=col('--accent');g.beginPath();g.arc(cx,Y(p[key]),3,2,Math.PI*2);g.fill();
      g.restore();
    }
  }
}
/* ---------- metrics: shared crosshair tooltip + drag-to-zoom ---------- */
S.metricZoom=null;S.crossIdx=null;S.dragSel=null;
function redrawCharts(){
  for(const id of ['#ccpu','#cmem']){
    const cv=$(id);if(!cv||!cv._cfg)continue;
    const c=cv._cfg;
    draw(cv,c.pts,c.key,c.fmt,c.limitKey,c.deploys,c.crashes,c.lineColor,c.label);
  }
}
const nearestIdx=(pts,t)=>{
  let best=0,bd=Infinity;
  for(let i=0;i<pts.length;i++){const d=Math.abs(pts[i].ts-t);if(d<bd){bd=d;best=i;}}
  return best;
};
function updateZoomChip(){
  const b=$('#zoomChip');if(!b)return;
  b.classList.toggle('on',!!S.metricZoom);
  if(S.metricZoom)b.textContent='⤢ '+fmtTime(S.metricZoom.t0).slice(0,5)+'–'+fmtTime(S.metricZoom.t1).slice(0,5)+' · reset';
}
function showChartTip(cv,clientX,clientY){
  const tip=$('#chartTip'),c=cv._cfg;
  if(!c||!c.pts.length||S.crossIdx==null)return;
  const rows=['#ccpu','#cmem'].map(id=>{
    const o=$(id);if(!o||!o._cfg||!o._cfg.pts.length)return null;
    const q=o._cfg.pts[Math.min(S.crossIdx,o._cfg.pts.length-1)];
    const val=o._cfg.fmt(Number(q[o._cfg.key]));
    return '<div class="tr"><span><span class="sw" style="background:var('+(o._cfg.lineColor||'--accent')+')"></span>'+
      (o._cfg.label||o.id)+'</span><b>'+val+'</b></div>';
  }).filter(Boolean).join('');
  tip.innerHTML='<div class="tt">'+fmtTime(c.pts[Math.min(S.crossIdx,c.pts.length-1)].ts)+'</div>'+rows;
  tip.style.display='block';
  const pad=14,tw=tip.offsetWidth,th=tip.offsetHeight;
  let x=clientX+pad,y=clientY+pad;
  if(x+tw>innerWidth-8)x=clientX-tw-pad;
  if(y+th>innerHeight-8)y=clientY-th-pad;
  tip.style.left=x+'px';tip.style.top=y+'px';
}
function hideChartTip(){const t=$('#chartTip');if(t)t.style.display='none';}
function wireChart(cv){
  if(!cv||cv._wired)return;cv._wired=true;
  let down=null;
  const pos=e=>{const r=cv.getBoundingClientRect();return e.clientX-r.left;};
  cv.addEventListener('pointermove',e=>{
    const c=cv._cfg,dom=cv._dom;
    if(!c||!dom||!c.pts.length)return;
    const px=pos(e);
    if(down){
      S.dragSel={cv:cv.id,a:down,b:px};
      redrawCharts();
      return;
    }
    if(px<dom.L||px>dom.w-dom.R){S.crossIdx=null;hideChartTip();redrawCharts();return;}
    const t=dom.x0+(px-dom.L)/Math.max(1,dom.w-dom.L-dom.R)*(dom.x1-dom.x0);
    S.crossIdx=nearestIdx(c.pts,t);
    redrawCharts();
    showChartTip(cv,e.clientX,e.clientY);
  });
  cv.addEventListener('pointerleave',()=>{
    if(down)return;
    S.crossIdx=null;hideChartTip();redrawCharts();
  });
  cv.addEventListener('pointerdown',e=>{
    if(e.button!==0)return;
    down=pos(e);
    try{cv.setPointerCapture(e.pointerId);}catch(err){/* capture is best-effort */}
  });
  cv.addEventListener('pointerup',e=>{
    if(down==null)return;
    const up=pos(e),from=down;
    down=null;S.dragSel=null;
    const dom=cv._dom;
    redrawCharts();
    if(!dom||Math.abs(up-from)<6)return; // a click, not a drag: keep the zoom
    const toT=px=>dom.x0+(px-dom.L)/Math.max(1,dom.w-dom.L-dom.R)*(dom.x1-dom.x0);
    S.metricZoom={t0:toT(Math.min(from,up)),t1:toT(Math.max(from,up))};
    S.crossIdx=null;hideChartTip();
    redrawCharts();updateZoomChip();
  });
  cv.addEventListener('dblclick',()=>{S.metricZoom=null;S.crossIdx=null;hideChartTip();redrawCharts();updateZoomChip();});
}
async function loadMetrics(){
  if(S.app===null)return;
  const p=new URLSearchParams();if(S.app)p.set('app',S.app);if(S.host)p.set('host',S.host);p.set('range',$('#range').value);if($('#mcont').value)p.set('container',$('#mcont').value);
  let d;
  try{d=await api('/api/metrics?'+p);}catch(e){const box=$('#ccpu');box.innerHTML='';const err=document.createElement('div');err.className='callout warn';err.textContent='Metrics failed to load: '+(e.message||e);box.append(err);return;}
  const deploys=await loadDeploys($('#range').value);
  const crashes=await loadCrashMarkers($('#range').value);
  draw($('#ccpu'),d.points,'cpu',v=>v.toFixed(0)+'%',null,deploys,crashes,'--ok','CPU %');
  // Memory as % of limit (like Dozzle)
  const memPts=d.points.map(p=>({...p,mem_pct:p.mem_limit>0?p.mem/p.mem_limit*100:0}));
  draw($('#cmem'),memPts,'mem_pct',v=>v.toFixed(1)+'%',null,deploys,crashes,'--accent','Memory');
  // Pointer handlers (crosshair + drag-zoom) are wired once per chart.
  wireChart($('#ccpu'));wireChart($('#cmem'));
  renderDeployList(deploys);
}
/* A new range/container means new samples: the old zoom window and crosshair
   no longer map onto them, so clear both before reloading. */
const reloadMetrics=()=>{S.metricZoom=null;S.crossIdx=null;updateZoomChip();loadMetrics();};
$('#range').onchange=reloadMetrics;$('#mcont').onchange=reloadMetrics;
$('#mdeploy').onclick=recordDeploy;
$('#zoomChip').onclick=()=>{S.metricZoom=null;S.crossIdx=null;hideChartTip();redrawCharts();updateZoomChip();};

/* ---------- deploy markers (V9) ---------- */
const RANGE_MS={'15m':15*60e3,'1h':3600e3,'6h':6*3600e3,'24h':24*3600e3,'7d':7*24*3600e3};
async function loadDeploys(range,sinceMs){
  const ms=RANGE_MS[range]||3600e3;const since=sinceMs||(Date.now()-ms);
  const p=new URLSearchParams();if(S.app)p.set('app',S.app);p.set('since',since);p.set('limit','200');
  try{const d=await api('/api/deploys?'+p);return (d.deploys||[]).slice().reverse();} // oldest→newest for drawing
  catch(e){console.warn('capscope: deploys load failed',e);return [];}
}
async function loadCrashMarkers(range,sinceMs){
  const ms=RANGE_MS[range]||3600e3;const since=sinceMs||(Date.now()-ms);
  const p=new URLSearchParams();if(S.app)p.set('app',S.app);if(S.host)p.set('host',S.host);p.set('since',since);p.set('limit','200');
  try{const d=await api('/api/snapshots?'+p);return (d.snapshots||[]).slice().reverse();} // oldest→newest for drawing
  catch(e){console.warn('capscope: crashes load failed',e);return [];}
}
/* Interleave deploy marker rows into ascending-ts log rows by timestamp. */
function mergeDeploys(rows,deploys){
  if(!deploys||!deploys.length)return rows;
  const out=[];let i=0;
  for(const r of rows){
    while(i<deploys.length&&deploys[i].ts<=r.ts){out.push(deployRow(deploys[i++]));}
    out.push(r);
  }
  while(i<deploys.length)out.push(deployRow(deploys[i++]));
  return out;
}
function deployRow(d){return {d:1,ts:d.ts,version:d.version,note:d.note,app:d.app};}
function renderDeployList(deploys){
  const host=$('#deploys');if(!host)return;
  host.replaceChildren();
  if(!deploys.length){host.style.display='none';return;}
  host.style.display='';
  const lab=document.createElement('span');lab.className='chip';lab.textContent=deploys.length+' deploy'+(deploys.length>1?'s':'')+' in range';
  host.append(lab);
  deploys.slice().reverse().forEach(d=>{ // newest first
    const c=document.createElement('span');c.className='k clean_exit';c.style.cursor='default';
    c.textContent=(d.version||d.app)+' · '+ago(d.ts);
    c.title=(d.version||'')+(d.note?'\n'+d.note:'')+'\n'+fmtDT(d.ts);
    host.append(c);
  });
}
async function recordDeploy(){
  if(!S.app){alert('Select an app first (deploy markers are per app).');return;}
  const version=prompt('Version or label for this deploy of "'+S.app+'":');
  if(version===null)return;
  try{
    const created=await api('/api/deploys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({app:S.app,version:version.trim()})});
    toast('Deploy "'+created.version+'" recorded for '+S.app,'ok');
    if(S.tab==='metrics')loadMetrics();
    if(S.tab==='logs')insertLiveDeployMarkers(created);
    if(S.tab==='crashes')loadCrashes();
  }catch(e){toast('Deploy record failed: '+(e.message||e),'err');}
}
/* Append a freshly recorded deploy as a marker row to the visible log pane. */
function insertLiveDeployMarkers(created){
  if(!created||!S.panes.length)return;
  const row=deployRow({ts:created.ts,version:created.version,note:created.note,app:created.app});
  for(const p of S.panes){if(p.vl)p.vl.append([row],true);}
  updateCnt();
}

/* ---------- crashes ---------- */
async function loadCrashes(){
  // incident permalink: #crash=<id> opens that snapshot directly
  const m=location.hash.match(/^#crash=(\d+)$/);
  if(m){await openCrash(m[1]);return;}
  const box=$('#view-crashes');box.innerHTML='';
  const only=S.crashOnlyApp&&S.app;
  let d;
  try{d=await api('/api/snapshots?limit=100'+(only?'&app='+encodeURIComponent(S.app):'')+(S.host?'&host='+encodeURIComponent(S.host):''));}catch(e){box.innerHTML='';const err=document.createElement('div');err.className='callout warn';err.textContent='Crashes failed to load: '+(e.message||e);box.append(err);return;}
  if(!d||!Array.isArray(d.snapshots)){console.warn('capscope: unexpected /api/snapshots shape',d);return;}
  const bar=document.createElement('div');bar.style.marginBottom='12px';
  const lab=document.createElement('label');lab.className='chk';
  const cb=document.createElement('input');cb.type='checkbox';cb.id='onlyapp';cb.checked=!!only;cb.onchange=()=>{S.crashOnlyApp=cb.checked;loadCrashes();};
  lab.append(cb,'only show '+(S.app||'selected app'));bar.append(lab);box.append(bar);
  if(!d.snapshots.length){const e=document.createElement('div');e.className='empty';e.textContent='No crash snapshots yet. They are saved automatically when a container dies with a non-zero exit code or is OOM killed.';box.append(e);S.lastCrashCount=0;return;}
  // Health-check snapshot toast: notify when new crash snapshots are detected.
  const prev=S.lastCrashCount||0;if(d.snapshots.length>prev){const latest=d.snapshots[d.snapshots.length-1];toast('Health-check snapshot #'+latest.id+' captured — '+latest.app+' · '+latest.kind.replace('_',' ')+' (exit '+latest.exit_code+')','ok');}S.lastCrashCount=d.snapshots.length;
  const tb=document.createElement('table');tb.innerHTML='<tr><th>When</th><th>App</th><th>Kind</th><th>Exit</th><th>Summary</th></tr>';
  d.snapshots.forEach(s=>{
    const tr=document.createElement('tr');tr.className='row';
    const cells=[fmtDT(s.ts)+' ('+ago(s.ts)+')',s.app,null,String(s.exit_code),s.summary];
    cells.forEach((c,i)=>{const td=document.createElement('td');if(i===2){const k=document.createElement('span');k.className='k '+s.kind;k.textContent=s.kind.replace('_',' ');td.append(k);
      if(s.restarts_window>=2){const rp=document.createElement('span');rp.className='k loop';rp.textContent='↻ '+s.restarts_window;rp.title=s.restarts_window+' deaths of this container inside the restart-loop window';rp.style.marginLeft='6px';td.append(rp);}
    }else{td.textContent=c;if(i===0)td.dataset.agoTs=s.ts;}tr.append(td);});
    tr.onclick=()=>openCrash(s.id);tb.append(tr);
  });
  box.append(crashTimeline(d.snapshots));
  const sec=secBlock('CRASH SNAPSHOTS',d.snapshots.length);
  sec.body.append(tb);
  box.append(sec.wrap);
}
/* 24h incident strip: one tick per crash, colored by kind, above the table.
   Clicking a tick opens that incident; ticks are buttons so they are also
   reachable from the keyboard. */
function crashTimeline(snaps){
  const now=Date.now(),lo=now-86400e3;
  const win=snaps.filter(s=>s.ts>=lo);
  const wrap=document.createElement('div');
  const cap=document.createElement('div');cap.className='tl-cap';
  const chip=document.createElement('span');chip.className='chip';
  chip.textContent=win.length+(win.length===1?' incident':' incidents')+' · last 24h';
  chip.title='Incidents in the last 24 hours ('+snaps.length+' total in this list)';
  cap.append(chip);wrap.append(cap);
  const tl=document.createElement('div');tl.className='tl';
  tl.setAttribute('role','list');
  tl.setAttribute('aria-label','Crash incidents in the last 24 hours');
  for(const s of win){
    const b=document.createElement('button');b.type='button';
    b.className='tick '+(s.kind||'crash');
    b.style.left=((s.ts-lo)/(now-lo)*100).toFixed(3)+'%';
    b.title=fmtDT(s.ts)+' — '+s.app+' · '+(s.kind||'').replace('_',' ')+' (exit '+s.exit_code+')'+(s.summary?'\n'+s.summary:'');
    b.setAttribute('aria-label','Open incident: '+b.title.replace(/\n/g,' '));
    b.onclick=()=>openCrash(s.id);
    tl.append(b);
  }
  const ax=document.createElement('div');ax.className='axis';
  [[0,'-24h'],[6,'-18h'],[12,'-12h'],[18,'-6h'],[24,'now']].forEach(([h,lab])=>{
    const sp=document.createElement('span');sp.textContent=lab;sp.style.left=(h/24*100)+'%';ax.append(sp);
  });
  tl.append(ax);
  wrap.append(tl);
  return wrap;
}
async function openCrash(id){
  let s;
  try{s=await api('/api/snapshots/'+id);}catch(e){const box=$('#view-crashes');box.innerHTML='';const err=document.createElement('div');err.className='callout warn';err.textContent='Crash failed to load: '+(e.message||e);box.append(err);return;}
  const box=$('#view-crashes');box.innerHTML='';
  // v0.7-D: incident card layout — action bar, summary, kv grid, chart, logs.
  const card=document.createElement('div');card.className='incident';
  const ibar=document.createElement('div');ibar.className='ibar';
  const back=document.createElement('button');back.className='b';back.textContent='← Back to list';back.onclick=()=>{location.hash='';loadCrashes();};
  const dl=document.createElement('a');dl.className='b';dl.href='/api/snapshots/'+id+'/export';dl.textContent='Download Markdown';dl.style.textDecoration='none';
  // Phase 6.3: self-contained HTML incident export (no assets, pastes into a ticket).
  const dlh=document.createElement('a');dlh.className='b';dlh.href='/api/snapshots/'+id+'/incident.html';dlh.textContent='Export HTML';dlh.style.textDecoration='none';
  // V10: expiring share link for this incident (capability token in the DB).
  const sh=document.createElement('button');sh.className='b';sh.textContent='Share 7d';
  sh.onclick=async()=>{try{const d=await api('/api/share',{method:'POST',body:JSON.stringify({hash:'crash='+s.id,ttl:'7d'})});const url=location.origin+d.url;try{await navigator.clipboard.writeText(url);}catch(e){prompt('Share link (expires in 7d):',url);}sh.textContent='Copied ✓';toast('Expiring incident link copied','ok');setTimeout(()=>sh.textContent='Share 7d',1500);}catch(e){toast('Share failed: '+(e.message||e),null);}};
  // v0.7: shareable incident permalink + copy-link
  const link=document.createElement('a');link.href='#crash='+s.id;link.textContent='Permalink';
  const cp=document.createElement('button');cp.className='b';cp.textContent='Copy link';
  cp.onclick=async()=>{try{await navigator.clipboard.writeText(location.origin+location.pathname+'#crash='+s.id);cp.textContent='Copied ✓';toast('Incident link copied','ok');setTimeout(()=>cp.textContent='Copy link',1500);}catch(e){prompt('Copy this incident link:',location.origin+location.pathname+'#crash='+s.id);}};
  // v0.7-D: one-click "copy as Markdown" of the whole incident for pasting into chat/tickets
  const cm=document.createElement('button');cm.className='b';cm.textContent='Copy as Markdown';
  cm.onclick=async()=>{
    const md=[
      '## Incident: '+s.app+' — '+s.kind.replace('_',' ')+' (exit '+s.exit_code+')',
      '- Time: '+fmtDT(s.ts)+(s.die_ts?' (died '+fmtDT(s.die_ts)+')':''),
      '- Container: '+s.container+(s.image?' · image '+s.image:''),
      '- Uptime before death: '+s.uptime_sec+'s · restart count '+s.restart_count,
      '- Memory limit: '+(s.mem_limit?mib(s.mem_limit):'none'),
      s.restarts_window>=2?'- ⚠ Restart loop: '+s.restarts_window+' deaths in window':'',
      s.deploy?'- Last deploy: '+(s.deploy.version||'')+' at '+fmtDT(s.deploy.ts)+' ('+Math.round((s.ts-s.deploy.ts)/60000)+'m before crash)':'',
      s.error?'- Docker error: '+s.error:'',
      '',s.summary,'','```',...s.logs.map(l=>`${fmtTime(l.ts)} ${l.stream==='e'?'err':'out'} ${l.line}`),'```',
      location.origin+location.pathname+'#crash='+s.id
    ].filter(x=>x!=='').join('\n');
    try{await navigator.clipboard.writeText(md);cm.textContent='Copied ✓';toast('Incident copied as Markdown','ok');setTimeout(()=>cm.textContent='Copy as Markdown',1500);}
    catch(e){prompt('Copy the incident Markdown:',md);}
  };
  // Crash card action buttons (demo parity): Logs before crash + Snapshot.
  // Restart container is intentionally omitted — CapScope is read-only toward Docker.
  const lbs=document.createElement('button');lbs.className='b';lbs.textContent='Logs before crash';
  lbs.onclick=()=>{location.hash='';setTab('logs');setTimeout(()=>{S.app=s.app;selectApp(s.app);const p=params({limit:500,before_id:0,since_id:0},s.container);$('#q').value='container:'+s.container;show();},80);};
  const sn=document.createElement('button');sn.className='b';sn.textContent='Snapshot';
  sn.onclick=async()=>{try{const d=await api('/api/snapshots/'+id+'/incident.html');const blob=new Blob([d],{type:'text/html'});const url=URL.createObjectURL(blob);const a=document.createElement('a');a.href=url;a.download='capscope-incident-'+s.id+'.html';a.click();URL.revokeObjectURL(url);toast('Incident snapshot downloaded','ok');}catch(e){toast('Snapshot download failed: '+(e.message||e),'err');}};
  ibar.append(back,cp,cm,dl,dlh,sh,link,lbs,sn);
  const h=document.createElement('h2');h.textContent=`${s.app}: ${s.kind.replace('_',' ')} (exit ${s.exit_code})`;
  const sum=document.createElement('div');sum.className='sum';sum.textContent=s.summary;
  const kv=document.createElement('div');kv.className='kv';
  const cells=[['Time',fmtDT(s.ts)],['Container',s.container],['Image',s.image],['Uptime',s.uptime_sec+'s'],['Restart count',s.restart_count],['Memory limit',s.mem_limit?mib(s.mem_limit):'none'],['Docker error',s.error||'none']];
  if(s.die_ts)cells.splice(1,0,['Died at',fmtDT(s.die_ts)]);
  if(s.restarts_window>=2)cells.push(['Deaths in window',s.restarts_window+' — restart loop']);
  if(s.deploy){
    const gapMs=s.ts-s.deploy.ts,gap=Math.round(gapMs/60000);
    const near=gapMs<15*60000; // deploy within 15 min before the crash
    cells.push(['Last deploy',(s.deploy.version||'')+' · '+fmtDT(s.deploy.ts)+(gap>=0?' ('+gap+'m before crash)':'')+(near?' — ⚠ recent deploy':'')]);
  }
  cells.forEach(([k,v])=>{const a=document.createElement('b'),b=document.createElement('span');a.textContent=k;b.textContent=v;kv.append(a,b);});
  const chart=document.createElement('div');chart.className='card';chart.innerHTML='<h3>Memory before death</h3><canvas id="cs"></canvas>';
  const lh=document.createElement('h3');lh.textContent=`Last ${s.logs.length} log lines`;lh.style.color='var(--muted)';
  const pre=document.createElement('pre');pre.className='logbox';
  pre.textContent=s.logs.map(l=>`${fmtTime(l.ts)} ${l.stream==='e'?'err':'out'} ${l.line}`).join('\n');
  card.append(ibar,h,sum,kv,chart,lh,pre);
  box.append(card);pre.scrollTop=pre.scrollHeight;
  draw($('#cs'),s.metrics,'mem',v=>mib(v),'mem_limit');
}
async function loadCrashCounts(){
  try{
    const d=await api('/api/snapshots?limit=200');const now=Date.now();S.appCrashes=new Map();
    for(const s of d.snapshots){if(now-s.ts<864e5){S.appCrashes.set(s.app,(S.appCrashes.get(s.app)||0)+1);}}
  }catch(e){console.warn('capscope: crash counts failed',e);}
}
async function crashChip(){
  await loadCrashCounts();let n=0;for(const v of S.appCrashes.values())n+=v;
  const c=$('#crashchip');c.style.display=n?'':'none';c.textContent=n+' crash'+(n>1?'es':'')+' in 24h';c.className='chip bad';
}
function sessionWarning(){
  if(!S.sessionExpires)return;
  const left=S.sessionExpires*1000-Date.now();
  const mins=Math.floor(left/60000);
  let el=$('#sessionWarn');
  if(mins<60&&mins>0){
    if(!el){el=document.createElement('div');el.id='sessionWarn';el.className='callout warn';el.style.margin='8px 16px';el.style.fontSize='12px';document.querySelector('#view-logs .bar').parentElement.insertBefore(el,document.querySelector('#view-logs .bar'));const btn=document.createElement('button');btn.className='b';btn.style.fontSize='11px';btn.style.marginLeft='8px';btn.textContent='Log in again';btn.onclick=showLogin;el.append(btn);}
    let msg=el.querySelector('.sessionWarnMsg');
    if(!msg){msg=document.createElement('span');msg.className='sessionWarnMsg';el.insertBefore(msg,el.firstChild);}
    msg.textContent='Session expires in '+mins+' min — ';
  }else if(el){el.remove();el=null;}
}
/* ---------- admin panel ---------- */
async function loadAdmin(){
  const box=$('#view-admin');
  if(S.role!=='admin')return;
  box.innerHTML='<div class="card"><p style="color:var(--muted);margin-top:0">Manage users, roles, and application scopes. Viewer accounts can access only explicitly assigned applications; admins have access to all. Passwords are never displayed.</p><div id="adminStatus" class="callout" style="display:none"></div><form id="userForm" style="display:flex;flex-wrap:wrap;gap:8px;align-items:end;margin:16px 0"><label style="display:flex;flex-direction:column;gap:4px">Username<input name="name" required autocomplete="off" placeholder="username"></label><label style="display:flex;flex-direction:column;gap:4px">Password<input name="password" required type="password" minlength="8" autocomplete="new-password" placeholder="at least 8 characters"></label><label style="display:flex;flex-direction:column;gap:4px">Role<select name="role"><option value="viewer">viewer</option><option value="admin">admin</option></select></label><button class="b" type="submit">Create user</button></form><table id="usersTable" style="width:100%"></table></div>';
  // Dozzle-style section header wraps the settings card (h2 lives in the header)
  const sec=secBlock('ADMIN',null);
  const card=box.firstElementChild;
  const h2=card.querySelector('h2');if(h2)h2.remove();
  sec.body.append(card);
  box.append(sec.wrap);
  const status=$('#adminStatus');
  const note=(msg,kind)=>{status.textContent=msg;status.className='callout '+(kind||'');status.style.display='';};
  const render=async()=>{
    try{
      const d=await api('/api/users');
      const table=$('#usersTable');table.replaceChildren();
      const head=document.createElement('tr');['User','Role','Applications','Created','Actions'].forEach(x=>{const th=document.createElement('th');th.textContent=x;head.append(th);});table.append(head);
      for(const u of d.users||[]){
        const tr=document.createElement('tr');
        const name=document.createElement('td');name.textContent=u.name;
        const role=document.createElement('td');const sel=document.createElement('select');['viewer','admin'].forEach(r=>{const o=document.createElement('option');o.value=r;o.textContent=r;o.selected=u.role===r;sel.append(o);});role.append(sel);
        const scope=document.createElement('td');scope.textContent=u.role==='admin'?'All applications':((u.apps||[]).join(', ')||'None');
        const scopeBtn=document.createElement('button');scopeBtn.className='b';scopeBtn.textContent='Edit apps';scopeBtn.disabled=u.role==='admin';scopeBtn.onclick=async()=>{const raw=prompt('Authorized applications (comma-separated):', (u.apps||[]).join(', '));if(raw===null)return;const apps=raw.split(',').map(x=>x.trim()).filter(Boolean);try{await api('/api/users/'+encodeURIComponent(u.id)+'/apps',{method:'PUT',body:JSON.stringify({apps})});u.apps=apps;scope.textContent=apps.join(', ')||'None';note('Application permissions updated','ok');}catch(e){note('Permission update failed: '+(e.message||e),'err');}};scope.append(document.createTextNode(' '),scopeBtn);
        const created=document.createElement('td');created.textContent=u.created_ms?fmtDT(u.created_ms):'—';
        const actions=document.createElement('td');
        sel.onchange=async()=>{try{await api('/api/users/'+encodeURIComponent(u.id)+'/role',{method:'POST',body:JSON.stringify({role:sel.value})});note('Role updated','ok');}catch(e){sel.value=u.role;note('Role update failed: '+(e.message||e),'err');}};
        if(u.name!==S.user){const del=document.createElement('button');del.className='b';del.textContent='Delete';del.onclick=async()=>{if(!confirm('Delete user '+u.name+'?'))return;try{await api('/api/users/'+encodeURIComponent(u.id),{method:'DELETE'});note('User deleted','ok');await render();}catch(e){note('Delete failed: '+(e.message||e),'err');}};actions.append(del);}else{actions.textContent='Current user';}
        tr.append(name,role,scope,created,actions);table.append(tr);
      }
    }catch(e){note('Could not load users: '+(e.message||e),'err');}
  };
  $('#userForm').onsubmit=async e=>{e.preventDefault();const f=new FormData(e.currentTarget);try{await api('/api/users',{method:'POST',body:JSON.stringify({name:f.get('name'),password:f.get('password'),role:f.get('role')})});e.currentTarget.reset();note('User created','ok');await render();}catch(ex){note('Create failed: '+(ex.message||ex),'err');}};
  await render();
}
/* ---------- Dozzle-style overview (v0.8) ----------
   GET /api/overview[?host=] → {host, window_ms, cpu, mem, containers[]}.
   hist = 10-minute window in ≤60 buckets {t0, step_ms, v[]}; avg/pk and the
   net/blk rates are 5-minute aggregates; container cpu numbers are
   docker-stats core-percent (100 = one full core), so avg_cpu/ncpu is the
   share of the whole host — the figure Dozzle's table shows. */
let dashTimer=0;
let dashGen=0;
let ovData=null;              // last payload: header clicks re-sort from it without refetching
// ovSort (table sort: column key + direction) is defined next to OV_COLS,
// restored from localStorage so the dashboard keeps the user's sort order.
function fmtBytes(b,dec){
  if(b==null||!isFinite(b)||b<0)return '—';
  if(b<1024)return Math.round(b)+' Bytes';
  const u=['KB','MB','GB','TB','PB'];let i=-1,v=b;
  do{v/=1024;i++;}while(v>=1024&&i<u.length-1);
  if(dec==null)dec=v<100?1:2;
  return v.toFixed(dec)+' '+u[i];
}
function fmtRate(v){ // bytes/second → "1.2 MB/s"
  if(v==null||!isFinite(v)||v<0)return '—';
  if(v<1)return v.toFixed(2)+' B/s';
  return fmtBytes(v,1)+'/s';
}
function fmtDur(sec){ // 123456 → "1d 10h"
  if(!isFinite(sec)||sec<0)return '—';
  sec=Math.round(sec);
  const d=Math.floor(sec/86400),h=Math.floor(sec%86400/3600),m=Math.floor(sec%3600/60);
  if(d)return d+'d'+(h?' '+h+'h':'');
  if(h)return h+'h'+(m?' '+m+'m':'');
  if(m)return m+'m';
  return sec+'s';
}
/* Dozzle bar chart: one rounded bar per bucket. Points are bucket-averaged
   down when more arrive than fit, scaled to peak*1.25 (or a fixed max), and
   coloured by how close the value is to `scale` (=100%):
   ≤50 green, ≤70 amber, ≤90 warn, above → red. */
function drawBars(cv,vals,o){
  o=o||{};
  if(!cv||!cv.getContext)return; // canvas detached mid-render
  const dpr=devicePixelRatio||1,w=Math.max(20,cv.clientWidth||160),h=Math.max(8,cv.clientHeight||18);
  cv.width=w*dpr;cv.height=h*dpr;
  const g=cv.getContext('2d');g.scale(dpr,dpr);
  const css=getComputedStyle(document.documentElement),col=n=>css.getPropertyValue(n).trim();
  g.clearRect(0,0,w,h);
  vals=(vals||[]).map(v=>Number(v)).filter(v=>isFinite(v));
  if(!vals.length){g.fillStyle=col('--muted');g.font='10px system-ui';g.fillText('no samples yet',3,h/2+3);return;}
  const gap=2,bw=3;
  const maxBars=Math.max(1,Math.floor((w+gap)/(bw+gap)));
  if(vals.length>maxBars){ // bucket-average down to the bars that fit
    const out=[],per=vals.length/maxBars;
    for(let i=0;i<maxBars;i++){
      const a=Math.floor(i*per),b=Math.max(a+1,Math.floor((i+1)*per));
      let s=0,c=0;for(let j=a;j<b&&j<vals.length;j++){s+=vals[j];c++;}
      out.push(c?s/c:0);
    }
    vals=out;
  }
  const n=vals.length,pitch=(w+gap)/n;
  const max=o.max||Math.max.apply(null,vals)*1.25||1;
  const scale=o.scale||100; // value that reads as 100% for colouring
  const color=v=>{const p=v/scale*100;return p>90?col('--err'):p>70?col('--warn'):p>50?col('--mem'):col('--ok');};
  g.lineWidth=Math.max(1,Math.min(bw,pitch-gap));
  g.lineCap='round';
  for(let i=0;i<n;i++){
    const v=vals[i],x=Math.round(i*pitch+pitch/2)+.5;
    const y=h-1-Math.max(0,Math.min(1,v/max))*(h-2);
    g.strokeStyle=color(v);
    g.beginPath();g.moveTo(x,h-1);g.lineTo(x,Math.max(y,1));g.stroke();
  }
}
/* Collapsible ▾ SECTION header (Dozzle): title + count badge, body toggles.
   The collapsed state persists per section title so a noisy panel stays out
   of the way across refreshes and page loads. */
function secBlock(title,count){
  const wrap=document.createElement('div');
  const head=document.createElement('div');head.className='sec';
  const chev=document.createElement('button');chev.className='chev';chev.textContent='▾';chev.title='Collapse section';
  const t=document.createElement('span');t.className='t';t.textContent=title;
  head.append(chev,t);
  if(count!=null){const c=document.createElement('span');c.className='cnt';c.textContent=count;head.append(c);}
  const sp=document.createElement('span');sp.className='sp';head.append(sp);
  const body=document.createElement('div');
  const key='sec:'+title;
  const apply=hid=>{body.style.display=hid?'none':'';chev.textContent=hid?'▸':'▾';chev.title=hid?'Expand section':'Collapse section';};
  apply(localStorage.getItem(key)==='1');
  chev.onclick=()=>{const hid=body.style.display==='none';apply(!hid);localStorage.setItem(key,hid?'0':'1');};
  wrap.append(head,body);
  return {wrap,body};
}
/* One host metric panel: title, current value, avg/peak line, bar chart. */
function metricPanel(kind,title,m,fmt,scale){
  const el=document.createElement('div');el.className='metric '+kind;
  const mh=document.createElement('div');mh.className='mh';
  const tt=document.createElement('span');tt.textContent=title;
  const cap=document.createElement('span');cap.className='cap';
  const hv=m&&m.hist&&m.hist.v||[];
  cap.textContent=hv.length&&m.hist.step_ms?Math.round(hv.length*m.hist.step_ms/60000)+' min':'10 min';
  mh.append(tt,cap);
  const mv=document.createElement('div');mv.className='mv';
  mv.textContent=m&&isFinite(Number(m.value))?fmt(Number(m.value)):'—';
  flashVal(mv,'mv:'+kind);
  const ms=document.createElement('div');ms.className='ms';
  if(m&&isFinite(Number(m.avg))){
    ms.innerHTML='avg <b></b> · peak <b></b>';
    const bs=ms.querySelectorAll('b');
    bs[0].textContent=fmt(Number(m.avg));
    bs[1].textContent=fmt(Number(m.pk));
  }else ms.textContent='no samples in this window';
  const cv=document.createElement('canvas');
  el.append(mh,mv,ms,cv);
  requestAnimationFrame(()=>drawBars(cv,hv,{scale}));
  return el;
}
/* HOSTS card: identity + facts + disk chips, then CPU / Memory panels. */
function hostCard(h){
  const card=document.createElement('div');card.className='hostcard';
  const hh=document.createElement('div');hh.className='hh';
  const ico=document.createElement('div');ico.className='hico';
  ico.innerHTML='<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><rect x="3" y="4" width="18" height="7" rx="2"/><rect x="3" y="13" width="18" height="7" rx="2"/><path d="M7 7.5h.01M7 16.5h.01"/></svg><span class="sdot"></span>';
  const nm=document.createElement('div');nm.className='hname';
  nm.textContent=h.name||h.id||'local host';
  if(h.id){nm.title='Filter everything to '+h.id;nm.onclick=()=>setHost(h.id);}
  const facts=document.createElement('div');facts.className='hfacts';
  const f=[h.ncpu?h.ncpu+' CPU':null,h.mem_total?fmtBytes(h.mem_total,1)+' RAM':null,
    h.docker_version?'Docker '+h.docker_version:null,h.uptime_s?fmtDur(h.uptime_s)+' up':null].filter(Boolean);
  facts.textContent=f.length?f.join(' · '):'No host metrics for this filter';
  const chips=document.createElement('div');chips.className='hchips';
  if(h.containers!=null){
    const hc=document.createElement('span');hc.className='hcount';
    hc.textContent=h.containers+(h.containers===1?' container':' containers');
    hc.title='Containers reporting from this host';
    chips.append(hc);
  }
  if(Number(h.disk_total)>0){
    const c=document.createElement('span');c.className='hchip';
    const usedPct=Math.max(0,Math.min(100,(1-Number(h.disk_free)/Number(h.disk_total))*100));
    c.innerHTML='Disk <b class="dv"></b><span class="track"><span class="fill" style="width:'+usedPct.toFixed(0)+'%"></span></span>';
    c.querySelector('.dv').textContent=fmtBytes(Number(h.disk_free),1)+' free';
    c.title=fmtBytes(Number(h.disk_free),1)+' free of '+fmtBytes(Number(h.disk_total),1)+' ('+usedPct.toFixed(0)+'% used)';
    chips.append(c);
  }
  if(Number(h.reclaimable)>0){
    const c=document.createElement('span');c.className='hchip';c.title='Docker data eligible for reclamation (dangling images, stopped containers, build cache)';
    const b=document.createElement('b');b.textContent=fmtBytes(Number(h.reclaimable),0);
    c.append(document.createTextNode('Reclaimable '),b);
    chips.append(c);
  }
  hh.append(ico,nm,facts,chips);
  card.append(hh);
  const grid=document.createElement('div');grid.className='mgrid';
  const cpu=h.cpu||{},mem=h.mem||{};
  grid.append(
    metricPanel('cpu','CPU',cpu,v=>v.toFixed(1)+'%',100),
    metricPanel('mem','Memory',mem,v=>fmtBytes(v,1),Number(h.mem_total)||0),
  );
  card.append(grid);
  return card;
}
/* Slim chips under the host card (from /api/dashboard stats). */
function statStrip(st){
  const s=document.createElement('div');s.className='statstrip';
  const chip=(val,label,title)=>{const c=document.createElement('span');c.className='schip';c.title=title||'';
    const b=document.createElement('b');b.textContent=val;
    flashVal(b,'stat:'+label);
    c.append(b,document.createTextNode(' '+label));return c;};
  s.append(
    chip(String(st.containers??'—'),'running','Live containers reported by Docker'),
    chip(String(st.restarts_24h??0),'restarts / 24h','Crash snapshots recorded in the last 24 hours'),
    chip(String(st.crash_loops??0),'crash loops','Apps with ≥3 deaths inside a restart-loop window'),
    chip(mib(st.db_bytes||0),'db size','SQLite storage in use'),
  );
  return s;
}
const OV_COLS=[
  {k:'name',t:'Name'},{k:'cpu',t:'CPU'},{k:'mem',t:'Memory'},
  {k:'net',t:'Network'},{k:'disk',t:'Disk'},{k:'started',t:'Created'},
];
/* Sort choice survives reloads; unknown keys fall back to the default so a
   removed column can't strand the table in a broken sort. */
const ovSort=(()=>{
  try{
    const o=JSON.parse(localStorage.getItem('ovSort')||'null');
    if(o&&typeof o.k==='string'&&typeof o.d==='number'&&(o.d===1||o.d===-1)&&OV_COLS.some(c=>c.k===o.k))return o;
  }catch(e){}
  return {k:'name',d:1};
})();
const saveOvSort=()=>localStorage.setItem('ovSort',JSON.stringify(ovSort));
function ovVal(c,k){
  switch(k){
    case 'cpu':return Number(c.avg_cpu||0);
    case 'mem':return Number(c.mem||0);
    case 'net':return Number(c.net_rx_rate||0)+Number(c.net_tx_rate||0);
    case 'disk':return Number(c.size_rw)>0?Number(c.size_rw):Number(c.blk_r_rate||0)+Number(c.blk_w_rate||0);
    case 'started':return Number(c.started||0);
    default:return String(c.name||'').toLowerCase();
  }
}
/* Metric cell fillers. Called once when a row is built and again on every
   overview poll, so a refresh can patch values in place (patchRow) instead
   of rebuilding the row: canvases keep their state, and flashVal highlights
   exactly the numbers that changed. */
function fillCpu(td,c){
  const ncpu=Number(c.ncpu)||Number(ovData&&ovData.host&&ovData.host.ncpu)||1;
  let inner=td.querySelector('.inner'),cv,v;
  if(!inner){
    inner=document.createElement('div');inner.className='inner';
    cv=document.createElement('canvas');v=document.createElement('span');v.className='v';
    inner.append(cv,v);td.append(inner);
  }else{cv=inner.querySelector('canvas');v=inner.querySelector('.v');}
  cv._hist=(c.cpu_hist&&c.cpu_hist.v)||[];
  cv._scale=ncpu*100;
  const share=Number(c.avg_cpu||0)/ncpu;
  v.textContent=isFinite(share)?share.toFixed(share>=10?1:2)+'%':'—';
  flashVal(v,'cpu:'+(c.id||c.name));
  const hv=cv._hist,pk=hv.length?Math.max.apply(null,hv)/ncpu:Number(c.cpu||0)/ncpu;
  td.title='avg '+share.toFixed(2)+'% of host · now '+(Number(c.cpu||0)/ncpu).toFixed(2)+'% · peak '+pk.toFixed(2)+'% (10 min)';
  requestAnimationFrame(()=>{if(td.isConnected)drawBars(cv,cv._hist,{scale:cv._scale});});
}
function fillMem(td,c){
  td.textContent=fmtBytes(Number(c.mem||0),1);
  if(Number(c.mem_limit)>0){
    const pc=Number(c.mem||0)/Number(c.mem_limit)*100;
    const sp=document.createElement('span');sp.style.color='var(--muted)';sp.style.fontSize='11px';
    sp.textContent=' ('+pc.toFixed(0)+'%)';td.append(sp);
  }
  td.title='now '+fmtBytes(Number(c.mem||0),1)+' · avg '+fmtBytes(Number(c.avg_mem||0),1)+' · limit '+(Number(c.mem_limit)>0?fmtBytes(Number(c.mem_limit),1):'unlimited');
  flashVal(td,'mem:'+(c.id||c.name));
}
function fillNet(td,c){
  let ri=td.querySelector('.in'),to=td.querySelector('.out');
  if(!ri){ri=document.createElement('div');ri.className='in';to=document.createElement('div');to.className='out';td.append(ri,to);}
  ri.title='receive rate (5 min)';ri.textContent='↓ '+fmtRate(Number(c.net_rx_rate));
  to.title='transmit rate (5 min)';to.textContent='↑ '+fmtRate(Number(c.net_tx_rate));
  flashVal(ri,'netrx:'+(c.id||c.name));flashVal(to,'nettx:'+(c.id||c.name));
}
function fillDisk(td,c){
  let dm=td.querySelector('.d1'),ds=td.querySelector('.d2');
  if(!dm){
    dm=document.createElement('div');dm.className='d1';
    ds=document.createElement('div');ds.className='d2';ds.style.fontSize='10.5px';ds.style.opacity='.75';
    td.append(dm,ds);
  }
  if(c.size_rw==null||Number(c.size_rw)<0){dm.textContent='—';dm.style.opacity='.55';}
  else{dm.textContent=fmtBytes(Number(c.size_rw),1);dm.style.opacity='';}
  ds.textContent='R '+fmtRate(Number(c.blk_r_rate))+' · W '+fmtRate(Number(c.blk_w_rate));
  td.title=(c.size_rw==null||Number(c.size_rw)<0)?'Read/write total not measured yet':'total read+write since start';
}
function fillCreated(td,c){
  td.textContent=c.started?ago(Number(c.started)):'—';
  if(c.started){td.title=fmtDT(Number(c.started));td.dataset.ago=Number(c.started);}
  else delete td.dataset.ago;
}
function patchRow(tr,c){
  const cs=tr._cells;
  if(!cs)return false;
  fillCpu(cs.cpu,c);fillMem(cs.mem,c);fillNet(cs.net,c);fillDisk(cs.disk,c);fillCreated(cs.cr,c);
  return true;
}
function ovRow(c,multi){
  const tr=document.createElement('tr');
  tr.title='Open logs for '+c.name;
  // Name: status dot + image icon + name, app/image as the sub-line
  const name=document.createElement('td');
  const cw=document.createElement('div');cw.className='c-name';
  const dot=document.createElement('span');dot.className='dot';
  const ico=document.createElement('span');ico.className='ico';ico.textContent=appIcon(c.image);
  const txt=document.createElement('div');txt.className='txt';
  const nm=document.createElement('div');nm.className='nm';nm.textContent=c.name;
  const sub=document.createElement('div');sub.className='sub';
  sub.textContent=(c.app||'')+(multi&&c.host_id?' · '+c.host_id:'');
  if(c.image){const im=document.createElement('span');im.className='mono';im.textContent=' · '+c.image;sub.append(im);}
  txt.append(nm,sub);cw.append(dot,ico,txt);name.append(cw);tr.append(name);
  // Metric cells: filled by the helpers above so later polls can patch them.
  const cpu=document.createElement('td');cpu.className='chart';fillCpu(cpu,c);
  const mem=document.createElement('td');mem.className='num';fillMem(mem,c);
  const net=document.createElement('td');net.className='net';fillNet(net,c);
  const disk=document.createElement('td');disk.className='disk';fillDisk(disk,c);
  const cr=document.createElement('td');cr.className='created';fillCreated(cr,c);
  tr.append(cpu,mem,net,disk,cr);
  tr._cells={cpu,mem,net,disk,cr};
  tr.dataset.id=c.id;
  // Hover actions: copy identifiers, jump to Metrics / Logs for this container.
  const mkAct=(glyph,title,fn)=>{
    const b=document.createElement('button');b.type='button';b.className='act';
    b.textContent=glyph;b.title=title;
    b.onclick=e=>{e.stopPropagation();fn();};
    return b;
  };
  const ac=document.createElement('td');ac.className='acts';
  const rowacts=document.createElement('span');rowacts.className='rowacts';
  rowacts.append(
    mkAct('⧉','Copy container name',()=>copyText(c.name||'','Container name copied')),
    mkAct('#','Copy container ID',()=>copyText(c.id||'','Container ID copied')),
    mkAct('▤','Open Metrics for '+c.name,()=>{selectContainer(c.id,c);setTab('metrics');}),
    mkAct('⏱','Open logs from '+c.name+'’s start',()=>{
      // selectContainer restarts the logs (a plain tail load); set the flag
      // after it and before setTab, because show() restarts again when the
      // logs tab is (re)entered — that second restart must be the one that
      // consumes the one-shot "from start" load, or it would clobber it.
      selectContainer(c.id,c);
      S.logSince={name:c.name,ts:Number(c.started)||0,fromStart:true};
      setTab('logs');
      toast('Loading logs from the start of '+c.name);
    }),
  );
  ac.append(rowacts);tr.append(ac);
  tr.onclick=()=>{selectContainer(c.id,c);if(S.tab!=='logs')setTab('logs');};
  return tr;
}
/* The containers table is windowed once it grows past VIRT_ROWS: only the
   rows visible in #view-dashboard sit in the DOM, between two spacer rows
   sized from a measured row height. Below the threshold every row is in the
   DOM and a refresh patches cells in place (patchRow) rather than rebuilding. */
const VIRT_ROWS=150;
let ctScroll=null; // exactly one active scroll listener: the view replaces itself
function renderCtable(){
  const wrap=document.createElement('div');wrap.className='tablewrap';
  const tb=document.createElement('table');tb.className='ctable';
  const thead=document.createElement('thead'),hr=document.createElement('tr');
  OV_COLS.forEach(c=>{
    const th=document.createElement('th');
    th.append(document.createTextNode(c.t));
    if(ovSort.k===c.k){th.classList.add('sorted');if(ovSort.d<0)th.classList.add('asc');}
    const arw=document.createElement('span');arw.className='arw';arw.textContent='▲';th.append(arw);
    th.title='Sort by '+c.t;
    th.onclick=()=>{if(ovSort.k===c.k)ovSort.d=-ovSort.d;else{ovSort.k=c.k;ovSort.d=c.k==='name'?1:-1;}saveOvSort();paint();};
    hr.append(th);
  });
  const hact=document.createElement('th');hact.className='acts';
  hact.setAttribute('aria-label','Row actions');hr.append(hact);
  thead.append(hr);tb.append(thead);
  const body=document.createElement('tbody');tb.append(body);
  wrap.append(tb);
  // Virtualization state: one scroll listener for the whole view, a measured
  // row height, and the rows/multi currently on screen.
  let rowH=0,curRows=[],curMulti=false;
  const spacer=n=>{
    const tr=document.createElement('tr');tr.className='vsp';tr.setAttribute('aria-hidden','true');
    const td=document.createElement('td');td.colSpan=OV_COLS.length+1;
    td.style.cssText='height:'+(n*rowH)+'px;padding:0;border:0';
    tr.append(td);return tr;
  };
  const drawWindow=()=>{
    const sc=$('#view-dashboard');
    const wr=wrap.getBoundingClientRect(),sr=sc.getBoundingClientRect();
    // Rows already scrolled past = how far the wrap top sits ABOVE the
    // viewport; the -4 fudge keeps the first partial row (and the header
    // row) rendered so nothing pops in late.
    const first=Math.max(0,Math.floor((sr.top-wr.top)/rowH)-4);
    const last=Math.min(curRows.length,first+Math.ceil(sr.height/rowH)+8);
    body.replaceChildren();
    if(first>0)body.append(spacer(first));
    const frag=document.createDocumentFragment();
    for(let i=first;i<last;i++)frag.append(ovRow(curRows[i],curMulti));
    body.append(frag);
    if(last<curRows.length)body.append(spacer(curRows.length-last));
  };
  const attachScroll=()=>{
    if(ctScroll)$('#view-dashboard').removeEventListener('scroll',ctScroll);
    const sc=$('#view-dashboard');
    let raf=0;
    ctScroll=()=>{
      if(!wrap.isConnected){if(ctScroll)$('#view-dashboard').removeEventListener('scroll',ctScroll);ctScroll=null;return;}
      if(raf)return;
      raf=requestAnimationFrame(()=>{raf=0;if(wrap.isConnected)drawWindow();});
    };
    sc.addEventListener('scroll',ctScroll,{passive:true});
  };
  // Two sample rows give the row height. A wide spread means rows are not
  // uniform — spacer math would drift, so return 0 and render everything.
  const measureRow=()=>{
    const idx=[0,Math.min(9,curRows.length-1)];
    body.replaceChildren(...idx.map(i=>ovRow(curRows[i],curMulti)));
    const hs=[...body.children].map(tr=>tr.offsetHeight);
    body.replaceChildren();
    if(hs.some(h=>!h))return 0;
    const avg=hs.reduce((a,b)=>a+b,0)/hs.length;
    return (Math.max(...hs)-Math.min(...hs))>avg*0.25?0:avg;
  };
  const renderAll=()=>{
    body.replaceChildren();
    const frag=document.createDocumentFragment();
    curRows.forEach(c=>frag.append(ovRow(c,curMulti)));
    body.append(frag);
    if(ctScroll)$('#view-dashboard').removeEventListener('scroll',ctScroll);
    ctScroll=null;
  };
  function paint(){
    const old=wrap.querySelector('.empty');if(old)old.remove();
    const rows=((ovData&&ovData.containers)||[]).slice();
    rows.sort((a,b)=>{
      const x=ovVal(a,ovSort.k),y=ovVal(b,ovSort.k);
      const r=typeof x==='string'?x.localeCompare(y):x-y;
      return (r||String(a.name||'').localeCompare(String(b.name||'')))*ovSort.d;
    });
    curRows=rows;curMulti=S.hosts.length>1;
    if(!rows.length){
      body.replaceChildren();
      if(ctScroll)$('#view-dashboard').removeEventListener('scroll',ctScroll);
      ctScroll=null;
      const e=document.createElement('div');e.className='empty';
      e.textContent='No containers are running'+(S.host?' on '+S.host:'')+'.';
      wrap.append(e);return;
    }
    // Same rows in the same order → patch the numbers where they stand: no
    // DOM churn, canvases keep their state, flashVal highlights what moved.
    if(rows.length<=VIRT_ROWS&&body.children.length===rows.length&&!body.querySelector('.vsp')
        &&rows.every((c,i)=>body.children[i].dataset.id===String(c.id))){
      rows.forEach((c,i)=>patchRow(body.children[i],c));
      return;
    }
    if(rows.length<=VIRT_ROWS){renderAll();return;}
    // Long list: window it. Measuring needs layout, so wait until attached.
    const start=()=>{
      if(!wrap.isConnected)return;
      if(!rowH)rowH=measureRow();
      if(!rowH){renderAll();return;}
      drawWindow();attachScroll();
    };
    if(wrap.isConnected)start();else requestAnimationFrame(start);
  }
  wrap.paint=paint;
  paint();
  return wrap;
}
async function loadOverview(){
  clearTimeout(dashTimer);
  const box=$('#view-dashboard');
  const host=S.host;
  const gen=++dashGen; // generation token: a slower, superseded fetch must not clobber a fresher render
  if(S.tab!=='dashboard'||S.app)return; // dashboard is the all-apps view only
  // First paint: shimmer skeleton. Background refreshes (15s poll) keep the
  // existing content on screen and swap it in atomically when data arrives,
  // so re-renders never flash "Loading…".
  const hasContent=box.childElementCount>0&&!box.firstElementChild?.classList.contains('sk-row');
  if(!hasContent)box.innerHTML='<div class="sk-row"><div class="sk sk-card"></div><div class="sk sk-card"></div><div class="sk sk-card"></div><div class="sk sk-card"></div></div><div class="sk-grid"><div class="sk sk-tile"></div><div class="sk sk-tile"></div><div class="sk sk-tile"></div></div>';
  const q=host?'?host='+encodeURIComponent(host):'';
  let d=null,st=null;
  try{
    const rs=await Promise.allSettled([api('/api/overview'+q),api('/api/dashboard'+q)]);
    if(rs[0].status==='fulfilled')d=rs[0].value;else throw rs[0].reason;
    if(rs[1].status==='fulfilled')st=rs[1].value&&rs[1].value.stats;
  }catch(e){
    if(gen!==dashGen)return;
    if(!hasContent){box.innerHTML='';const err=document.createElement('div');err.className='callout warn';err.textContent='Dashboard failed to load: '+(e.message||e);box.append(err);}
    return;
  }
  // A newer loadOverview has started (host switch / tab re-entry): discard this stale result.
  if(gen!==dashGen||S.tab!=='dashboard'||S.app||S.host!==host)return;
  if(!d||typeof d!=='object'){box.innerHTML='<div class="callout warn">Overview returned unexpected data.</div>';return;}
  ovData=d;
  box.innerHTML='';
  // HOSTS — a clickable card grid when several hosts report into this view,
  // otherwise the single host card. Clicking a card filters the whole app to
  // that host (click the active one again to return to "All hosts").
  const nHosts=(d.hosts&&d.hosts.length)||0;
  const hs=secBlock('HOSTS',nHosts||1);
  if(nHosts>1){
    const hgrid=document.createElement('div');hgrid.className='hostsgrid';
    for(const e of d.hosts){
      const card=hostCard(e);
      if(S.host===e.id)card.classList.add('on');
      card.title='Show everything on '+(e.name||e.id);
      card.onclick=ev=>{if(ev.target.closest('.hname'))return;setHost(S.host===e.id?'':e.id);};
      hgrid.append(card);
    }
    hs.body.append(hgrid);
  }else{
    hs.body.append(hostCard(d.host||{}));
  }
  if(st)hs.body.append(statStrip(st));
  box.append(hs.wrap);
  // CONTAINERS — sortable Dozzle-style table. The table itself outlives the
  // section: re-parenting the same element means a poll patches the rows in
  // place (canvases, sort, window and measured row height all survive).
  const list=d.containers||[];
  const cs=secBlock('CONTAINERS',list.length);
  if(!box._ct)box._ct=renderCtable();
  cs.body.append(box._ct);
  box._ct.paint();
  box.append(cs.wrap);
  // Recent crashes (24h) — fetch snapshots directly: loadCrashes() renders the
  // crashes view and returns undefined, so it can't supply data here
  const snaps=((await api('/api/snapshots?limit=100'+(host?'&host='+encodeURIComponent(host):'')).catch(()=>({}))).snapshots||[]).filter(sn=>sn.ts&&Date.now()-sn.ts<864e5);
  if(snaps.length){
    const rs2=secBlock('RECENT CRASHES (24H)',snaps.length);
    const line=document.createElement('div');line.style.display='flex';line.style.gap='8px';line.style.flexWrap='wrap';line.style.padding='2px 0 8px';
    snaps.slice().reverse().slice(0,15).forEach(sn=>{
      const chip=document.createElement('span');chip.className='k '+sn.kind;chip.style.cursor='pointer';
      chip.textContent=`${sn.app} · ${sn.kind.replace('_',' ')} · exit ${sn.exit_code}${sn.restarts_window>=2?' ↻'+sn.restarts_window:''}`;
      chip.title=fmtDT(sn.ts)+' — open incident permalink';
      chip.onclick=()=>{location.hash='crash='+sn.id;setTab('crashes');};
      line.append(chip);
    });
    rs2.body.append(line);
    box.append(rs2.wrap);
  }
  // auto-refresh while visible
  dashTimer=setTimeout(()=>{if(S.tab==='dashboard' && !S.app)loadOverview();},15000);
}
function drawSpark(canvas,pts){
  if(!canvas||!canvas.getContext)return; // guard: canvas detached mid-render
  const dpr=devicePixelRatio||1,w=canvas.clientWidth,h=canvas.clientHeight;
  canvas.width=w*dpr;canvas.height=h*dpr;const g=canvas.getContext('2d');g.scale(dpr,dpr);
  const css=getComputedStyle(document.documentElement);const col=n=>css.getPropertyValue(n).trim();
  g.clearRect(0,0,w,h);
  if(pts.length<2){g.fillStyle=col('--muted');g.font='11px system-ui';g.fillText('no metric samples yet',6,h/2);return;}
  const L=4,R=4,T=6,B=6,x0=pts[0].ts,x1=pts[pts.length-1].ts||x0+1;
  // memory % and cpu % both 0..100, so max=100 gives a shared scale
  const max=100;
  const X=t=>L+(t-x0)/(x1-x0||1)*(w-L-R),Y=v=>T+(1-v/max)*(h-T-B);
  // memory area as % of limit
  g.beginPath();g.moveTo(X(pts[0].ts),h-B);
  pts.forEach(p=>{const mp=p.mem_limit>0?p.mem/p.mem_limit*100:0;g.lineTo(X(p.ts),Y(mp));});
  g.lineTo(X(x1),h-B);g.closePath();
  g.fillStyle=col('--accent')+'33';g.fill();
  g.beginPath();pts.forEach((p,i)=>{const mp=p.mem_limit>0?p.mem/p.mem_limit*100:0;i?g.lineTo(X(p.ts),Y(mp)):g.moveTo(X(p.ts),Y(mp));});
  g.strokeStyle=col('--accent');g.lineWidth=1.5;g.stroke();
  // cpu line (0..100, same box)
  g.beginPath();pts.forEach((p,i)=>i?g.lineTo(X(p.ts),Y(p.cpu)):g.moveTo(X(p.ts),Y(p.cpu)));
  g.strokeStyle=col('--ok');g.lineWidth=1;g.setLineDash([3,3]);g.stroke();g.setLineDash([]);
}

/* ---------- alerts ---------- */
const ALERT_KIND={deadman:'Dead man',restart_loop:'Restart loop',metric:'Metric',log_pattern:'Log pattern',event:'Event'};
async function loadAlerts(){
  const box=$('#view-alerts');box.innerHTML='';
  let meta={rules:[],channels:[],enabled:false},hist={alerts:[]};
  try{meta=await api('/api/alerts/rules');}catch(e){connBad('alerts meta failed');}
  try{hist=await api('/api/alerts?limit=100');}catch(e){connBad('alerts history failed');}
  const now=Date.now();
  S.alertStatus=new Map();
  meta.rules.forEach(r=>{S.alertStatus.set(r.name, hist.alerts.some(a=>a.rule===r.name&&now-a.ts<3e5)?'firing':'resolved');});
  const bar=document.createElement('div');bar.className='bar';bar.style.marginBottom='12px';
  const t=document.createElement('button');t.className='b';t.textContent='Send test alert';
  t.disabled=!meta.channels.length;
  t.title=meta.channels.length?'Send a test message to: '+meta.channels.join(', '):'No notification channels configured (set ALERT_WEBHOOK_URL etc.)';
  t.onclick=async()=>{
    try{const d=await api('/api/alerts/test',{method:'POST'});
      alert('Channel results:\n'+Object.entries(d.results).map(([k,v])=>k+': '+v).join('\n'));
    }catch(e){alert('Test failed: '+(e.message||e));}
  };
  bar.append(t);box.append(bar);

  const rsec=secBlock('RULES',meta.rules.length);
  const rc=document.createElement('div');rc.className='card';
  const addBtn=document.createElement('button');addBtn.className='b';addBtn.style.cssText='font-size:11px;padding:2px 8px';addBtn.textContent='+ New rule';
  addBtn.disabled=!meta.enabled&&!meta.rules.length;
  addBtn.title='Create an alert rule in the browser (writes ALERTS_FILE via the API)';
  addBtn.onclick=()=>openRuleBuilder(meta,hist,loadAlerts);
  rsec.wrap.querySelector('.sec').append(addBtn);
  if(!meta.rules.length){
    const p=document.createElement('div');p.className='empty';
    p.textContent=meta.enabled?'No rules loaded. Click "+ New rule" to create one, or edit the JSON rules file (ALERTS_FILE) directly.':'Alerting is off: no rules file found. Click "+ New rule" to create the first one.';
    rc.append(p);
  }else{
    // One condition string for both the wide table (desktop) and the card
    // grid (narrow screens), so the two can never drift apart.
    const condOf=r=>r.kind==='deadman'?`no logs for ${r.for}`
      :r.kind==='restart_loop'?`> ${r.count} deaths in ${r.window}`
      :r.kind==='metric'?`${r.metric} ${r.op} ${r.value}${r.window?' avg of '+r.window:' for '+r.for}`
      :r.kind==='event'?`Docker event ${r.event}${r.status&&r.status!=='unhealthy'?' where status='+r.status:''}${r.exclude&&r.event==='die'?' except exit '+r.exclude.join('/'):''}`
      :`"${r.pattern}" ≥ ${r.hits} hits in ${r.window}`;
    const muteBtns=(r,muted)=>{
      const box=document.createElement('span');box.style.cssText='display:inline-flex;gap:6px';
      const m1=document.createElement('button');m1.className='b';m1.style.fontSize='10px';m1.style.padding='2px 6px';m1.textContent='1h';m1.title='Mute '+r.name+' for 1 hour';
      m1.onclick=()=>muteRule(r.name,3600e3);
      const m24=document.createElement('button');m24.className='b';m24.style.fontSize='10px';m24.style.padding='2px 6px';m24.textContent='24h';m24.title='Mute '+r.name+' for 24 hours';
      m24.onclick=()=>muteRule(r.name,86400e3);
      const un=document.createElement('button');un.className='b';un.style.fontSize='10px';un.style.padding='2px 6px';un.textContent='unmute';un.style.display=muted?'':'none';un.title='Unmute '+r.name;
      un.onclick=()=>{S.alertMuted.delete(r.name);loadAlerts();};
      box.append(m1,m24,un);if(muted){m1.style.display='none';m24.style.display='none';}
      return box;
    };
    const editBtn=r=>{
      const ed=document.createElement('button');ed.className='b';ed.style.cssText='font-size:10px;padding:2px 6px';ed.textContent='edit';ed.title='Edit '+r.name+' in the rule builder';
      ed.onclick=()=>openRuleBuilder(meta,hist,loadAlerts,meta.rules.indexOf(r));
      return ed;
    };
    const tb=document.createElement('table');tb.className='rules-wide';tb.innerHTML='<tr><th>Name</th><th>Kind</th><th>Apps</th><th>Condition</th><th>Repeat</th><th>Status</th><th>Mute</th></tr>';
    meta.rules.forEach(r=>{
      const tr=document.createElement('tr');
      const cond=condOf(r);
      const st=S.alertStatus.get(r.name)||'resolved';
      const muted=S.alertMuted.get(r.name,0)>now;
      const nameTd=document.createElement('td');nameTd.textContent=r.name;if(muted)nameTd.classList.add('muted');tr.append(nameTd);
      [[ALERT_KIND[r.kind]||r.kind],[(r.apps||[]).join(', ')||'all'],[cond],[r.repeat||'']].flat().forEach(v=>{const td=document.createElement('td');td.textContent=v;if(muted)td.classList.add('muted');tr.append(td);});
      const stTd=document.createElement('td');const dot=document.createElement('span');dot.className='status-dot '+st;dot.title=st;stTd.append(dot,document.createTextNode(' '+st));tr.append(stTd);
      const muTd=document.createElement('td');muTd.append(muteBtns(r,muted));tr.append(muTd);
      const edTd=document.createElement('td');edTd.append(editBtn(r));tr.append(edTd);
      tb.append(tr);
    });
    rc.append(tb);
    // Narrow screens: a card grid instead of the 7-column table, with firing
    // rules pinned to the top and a pulsing dot so they can't be missed.
    const cards=document.createElement('div');cards.className='rulecards';
    const rank=r=>(S.alertStatus.get(r.name)==='firing'?0:1);
    meta.rules.slice().sort((a,b)=>rank(a)-rank(b)).forEach(r=>{
      const st=S.alertStatus.get(r.name)||'resolved';
      const muted=S.alertMuted.get(r.name,0)>now;
      const card=document.createElement('div');
      card.className='rulecard'+(st==='firing'?' firing':'');
      const h=document.createElement('div');h.className='rc-h';
      if(st==='firing'){const pd=document.createElement('span');pd.className='pdot';pd.title='Firing now';h.append(pd);}
      const nm=document.createElement('span');nm.className='rc-n';nm.textContent=r.name;nm.title=r.name;
      const kd=document.createElement('span');kd.className='rc-k';kd.textContent=ALERT_KIND[r.kind]||r.kind;
      const sy=document.createElement('span');sy.className='rc-s';
      const dot=document.createElement('span');dot.className='status-dot '+st;dot.title=st;
      sy.append(dot,document.createTextNode(st+(muted?' · muted':'')));
      h.append(nm,kd,sy);
      const m=document.createElement('div');m.className='rc-m';
      const ap=document.createElement('span');ap.className='rc-k';ap.textContent=(r.apps||[]).join(', ')||'all apps';
      const cd=document.createElement('span');cd.textContent=condOf(r);
      const rp=document.createElement('span');rp.className='rc-k';rp.textContent=r.repeat||'no repeat';rp.title='Repeat interval';
      m.append(ap,cd,rp);
      const b=document.createElement('div');b.className='rc-b';
      b.append(muteBtns(r,muted),editBtn(r));
      card.append(h,m,b);
      cards.append(card);
    });
    rc.append(cards);
  }
  rsec.body.append(rc);
  box.append(rsec.wrap);

  hist={alerts:[]};
  try{hist=await api('/api/alerts?limit=100');}catch(e){connBad('alerts history failed');}
  const asec=secBlock('HISTORY',hist.alerts.length);
  const ac=document.createElement('div');ac.className='card';ac.style.marginTop='0';
  if(!hist.alerts.length){
    const p=document.createElement('div');p.className='empty';p.textContent='No alerts have fired yet.';ac.append(p);
  }else{
    const tb=document.createElement('table');tb.innerHTML='<tr><th>When</th><th>Rule</th><th>App</th><th>Kind</th><th>Message</th><th>Delivery</th></tr>';
    hist.alerts.forEach(a=>{
      const tr=document.createElement('tr');
      const cells=[fmtDT(a.ts)+' ('+ago(a.ts)+')',a.rule,a.app,ALERT_KIND[a.kind]||a.kind,a.message];
      cells.forEach((v,i)=>{const td=document.createElement('td');td.textContent=v;if(i===0)td.dataset.agoTs=a.ts;tr.append(td);});
      // v0.7: delivery status recorded by the alert engine after notify().
      const dTd=document.createElement('td');
      const dmap={sent:['✓ sent','var(--ok)'],failed:['✗ failed','var(--err)']};
      const dv=dmap[a.delivery];
      if(dv){dTd.textContent=dv[0];dTd.style.color=dv[1];dTd.style.fontWeight='600';dTd.title=a.delivery==='sent'?'At least one channel accepted the message':'All channels errored — nobody was notified';}
      else{dTd.textContent='—';dTd.style.color='var(--muted)';dTd.title='No notification channels configured for this firing (or recorded before v0.7)';}
      tr.append(dTd);
      tb.append(tr);
    });
    ac.append(tb);
  }
  asec.body.append(ac);
  box.append(asec.wrap);
}

/* ---------- alert rule builder (writes ALERTS_FILE via the API) ---------- */
function ruleToForm(r){return{name:r.name||'',kind:r.kind||'deadman',apps:(r.apps||[]).join(', '),
  for:r.for||'',count:r.count??'',window:r.window||'',metric:r.metric||'cpu',op:r.op||'gt',value:r.value??'',
  pattern:r.pattern||'',hits:r.hits??'',repeat:r.repeat||'',event:r.event||'die',status:r.status||'',
  exclude:(r.exclude||[]).join(', '),cooldown:r.cooldown||''};}
function formToRule(f){
  const r={name:f.name.trim(),kind:f.kind};
  if(f.apps.trim())r.apps=f.apps.split(',').map(s=>s.trim()).filter(Boolean);
  const dur=['for','window','repeat','cooldown'];dur.forEach(k=>{if(f[k]&&f[k].trim())r[k]=f[k].trim();});
  if(r.kind==='restart_loop'){if(f.count!=='')r.count=+f.count;}
  if(r.kind==='metric'){r.metric=f.metric;r.op=f.op;if(f.value!=='')r.value=+f.value;}
  if(r.kind==='log_pattern'){if(f.pattern)r.pattern=f.pattern;if(f.hits!=='')r.hits=+f.hits;}
  if(r.kind==='event'){r.event=f.event;
    if(f.status&&f.status.trim())r.status=f.status.trim();
    if(r.event==='die')r.exclude=f.exclude.split(',').map(s=>s.trim()).filter(Boolean).map(Number).filter(n=>Number.isFinite(n));}
  return r;}
function openRuleBuilder(meta,hist,onSaved,editIdx){
  document.getElementById('ruleModal')?.remove();
  const rules=(meta.rules||[]).map(ruleToForm);
  let idx=(editIdx==null)?rules.length:editIdx;
  let form=idx<rules.length?Object.assign({},rules[idx]):ruleToForm({kind:'deadman'});
  const overlay=document.createElement('div');overlay.id='ruleModal';
  overlay.style.cssText='position:fixed;inset:0;background:rgba(0,0,0,.55);display:flex;align-items:center;justify-content:center;z-index:1000';
  const box=document.createElement('div');box.className='card';
  box.style.cssText='width:min(680px,92vw);max-height:88vh;overflow:auto;border-width:3px;box-shadow:6px 6px 0 rgba(0,0,0,.35)';
  const mk=(tag,attrs,...kids)=>{const e=document.createElement(tag);Object.assign(e,attrs);kids.forEach(k=>e.append(k));return e;};
  const field=(label,input)=>mk('label',{style:'display:block;margin:8px 0;font-size:12px'},mk('span',{textContent:label,style:'display:block;margin-bottom:3px;color:var(--muted)'}),input);
  const inp=(key,val,ph)=>mk('input',{value:val??'',placeholder:ph||'',oninput:e=>form[key]=e.target.value,style:'width:100%'});
  const kindSel=mk('select',{style:'width:100%',onchange:e=>{form.kind=e.target.value;drawFields();}});
  [['deadman','Dead man — app goes silent'],['restart_loop','Restart loop — too many deaths'],['metric','Metric — cpu/mem threshold'],['log_pattern','Log pattern — regex hits'],['event','Container event — die / OOM / health']].forEach(([v,l])=>kindSel.append(mk('option',{value:v,textContent:l})));
  kindSel.value=form.kind;
  const condWrap=mk('div',{});
  function drawFields(){
    condWrap.innerHTML='';
    const grid=mk('div',{style:'display:grid;grid-template-columns:1fr 1fr;gap:0 12px'});
    if(form.kind==='deadman'){grid.append(field('Silence duration that fires (e.g. 5m)',inp('for',form.for,'5m')));}
    else if(form.kind==='event'){
      const esel=mk('select',{style:'width:100%',onchange:e=>{form.event=e.target.value;drawFields();}});
      [['die','Container died'],['oom','Container OOM-killed'],['health_status','Health status change'],['update_available','Update available']].forEach(([v,l])=>esel.append(mk('option',{value:v,textContent:l})));esel.value=form.event;
      grid.append(field('Docker event',esel));
      if(form.event==='die'){grid.append(field('Ignored exit codes (comma list, default 0,130,143,137)',inp('exclude',form.exclude,'0,130,143,137')));}
      if(form.event==='health_status'){grid.append(field('Status value that fires (e.g. unhealthy)',inp('status',form.status,'unhealthy')));}
    }
    else if(form.kind==='restart_loop'){
      grid.append(field('Deaths (>N)',inp('count',form.count,'3')),field('Window (e.g. 10m)',inp('window',form.window,'10m')));
    }else if(form.kind==='metric'){
      const msel=mk('select',{style:'width:100%',onchange:e=>form.metric=e.target.value});[['cpu','cpu %'],['mem','memory bytes'],['mem_pct','memory % of limit']].forEach(([v,l])=>msel.append(mk('option',{value:v,textContent:l})));msel.value=form.metric;
      const osel=mk('select',{style:'width:100%',onchange:e=>form.op=e.target.value});[['gt','> above'],['lt','< below']].forEach(([v,l])=>osel.append(mk('option',{value:v,textContent:l})));osel.value=form.op;
      grid.append(field('Metric',msel),field('Operator',osel),field('Threshold',inp('value',form.value,'80')),field('Hold for (e.g. 2m)',inp('for',form.for,'5m')));
    }else{
      grid.append(field('Regex pattern',inp('pattern',form.pattern,'panic|fatal')),field('Hits (≥N)',inp('hits',form.hits,'1')),field('Window (e.g. 10m)',inp('window',form.window,'10m')));
    }
    grid.append(field('Notify again after (silence)',inp('repeat',form.repeat,'30m')));
    condWrap.append(grid);
  }
  drawFields();
  const head=mk('h3',{style:'margin:0 0 4px'},idx<rules.length?'Edit rule':'New alert rule');
  const nameRow=field('Rule name (unique)',mk('input',{value:form.name,style:'width:100%',oninput:e=>form.name=e.target.value}));
  // JSON preview + live validation
  const pre=mk('pre',{style:'background:var(--bg);border:1px solid var(--line);border-radius:6px;padding:8px;font-size:11px;min-height:44px;white-space:pre-wrap;margin:8px 0'});
  const status=mk('div',{style:'font-size:12px;min-height:18px;margin:4px 0'});
  let vT;
  function refreshPreview(){
    try{pre.textContent=JSON.stringify([formToRule(form)],null,2);}catch(e){pre.textContent='// invalid: '+e;}
    clearTimeout(vT);vT=setTimeout(async()=>{
      try{await api('/api/alerts/rules/validate',{method:'POST',body:JSON.stringify({rules:[formToRule(form)]})});
        status.textContent='✓ valid';status.style.color='var(--ok,#22a06b)';saveBtn.disabled=false;}
      catch(e){status.textContent='✗ '+(e.message||e);status.style.color='#d33';saveBtn.disabled=true;}
    },350);
  }
  box.oninput=refreshPreview;
  const listRow=mk('div',{style:'display:flex;gap:6px;flex-wrap:wrap;margin:6px 0 2px'});
  (rules.length?rules.map((r,i)=>mk('button',{className:'b',style:'font-size:11px;padding:2px 8px'+(i===idx?';outline:2px solid var(--accent)':''),textContent:r.name||'(unnamed)',onclick:()=>{idx=i;form=Object.assign({},rules[i]);kindSel.value=form.kind;nameRow.querySelector('input').value=form.name;drawFields();refreshPreview();}})):[]).forEach(b=>listRow.append(b));
  listRow.append(mk('button',{className:'b',style:'font-size:11px;padding:2px 8px',textContent:'+ blank',onclick:()=>{idx=rules.length;form=ruleToForm({kind:'deadman'});kindSel.value='deadman';nameRow.querySelector('input').value='';drawFields();refreshPreview();}}));
  const saveBtn=mk('button',{className:'b',style:'font-weight:700',textContent:'Save rules'});
  saveBtn.onclick=async()=>{
    const next=rules.slice();next[idx]=form;
    const doc={rules:next.map(formToRule)};
    try{
      await api('/api/alerts/rules',{method:'POST',body:JSON.stringify(doc)});
      toast('Alert rules saved — engine hot-reloaded','ok');
      overlay.remove();onSaved&&onSaved();
    }catch(e){status.textContent='✗ save failed: '+(e.message||e);status.style.color='#d33';}
  };
  const testBtn=mk('button',{className:'b',style:'font-weight:600',textContent:'Test fire'});
  testBtn.onclick=async()=>{
    const rule=formToRule(form);
    try{
      const res=await api('/api/alerts/test-rule',{method:'POST',body:JSON.stringify({rule:rule,limit:50})});
      toast('Test fire: '+(res.firing? 'FIRING �?':'not firing �?')+' ('+res.matchingRows+' matches)','ok');
      pre.textContent=JSON.stringify(res,null,2);
    }catch(e){toast('Test fire failed: '+(e.message||e),'err');}
  };
  const delBtn=idx<rules.length?mk('button',{className:'b',textContent:'Delete',style:'color:#d33',onclick:async()=>{
    const next=rules.filter((_,i)=>i!==idx);
    try{await api('/api/alerts/rules',{method:'POST',body:JSON.stringify({rules:next.map(formToRule)})});toast('Rule deleted','ok');overlay.remove();onSaved&&onSaved();}
    catch(e){status.textContent='✗ delete failed: '+(e.message||e);status.style.color='#d33';}
  }}):null;
  const cancel=mk('button',{className:'b',textContent:'Cancel',onclick:()=>overlay.remove()});
  box.append(head,mk('div',{style:'font-size:11px;color:var(--muted)',textContent:'Saved rules replace the ALERTS_FILE contents and hot-reload into the engine.'}),listRow,nameRow,field('Kind',kindSel),condWrap,pre,status,
    mk('div',{style:'display:flex;gap:8px;margin-top:8px'},saveBtn,cancel,...(delBtn?[delBtn]:[])));
  overlay.append(box);
  overlay.onclick=e=>{if(e.target===overlay)overlay.remove();};
  document.body.append(overlay);
  refreshPreview();
}

function muteRule(name,forMs){
  S.alertMuted.set(name,Date.now()+forMs);
  loadAlerts();
}

addEventListener('DOMContentLoaded',()=>{
  async function checkSession(){
    try{
      const d=await api('/api/session');
      // auth_enabled:false => server runs without AUTH_PASS; proceed anonymously.
      return{ok:true,user:d.user||'anonymous',role:d.role||'viewer',expiresAt:d.expires_at||0,authEnabled:d.auth_enabled!==false};
    }catch(e){return{ok:false};}
  }
  $('#loginForm').onsubmit=async(e)=>{
    e.preventDefault();
    const err=$('#loginError');err.style.display='none';
    try{
      const d=await api('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({
        username:$('#loginUser').value,password:$('#loginPass').value
      })});
      localStorage.setItem('capscope_token',d.token);
      $('#loginUser').value='';$('#loginPass').value='';
      hideLogin();
      refreshApps().then(()=>{crashChip();show();});
    }catch(ex){err.textContent='Invalid username or password.';err.style.display='';}
  };
  $('#loginReveal').onclick=()=>{
    const i=$('#loginPass');const shown=i.type==='text';
    i.type=shown?'password':'text';
    $('#loginReveal').innerHTML=shown?'<i data-lucide="eye"></i>':'<i data-lucide="eye-off"></i>';
    window.lucide&&window.lucide.createIcons&&window.lucide.createIcons({nodes:[$('#loginReveal').firstElementChild]});
  };
  $('#logoutBtn').onclick=async()=>{
    try{await api('/api/logout',{method:'POST'});}catch(e){}
    localStorage.removeItem('capscope_token');
    location.reload();
  };
  (async()=>{
    const q=new URLSearchParams(location.search);
    if(q.get('app')!==null)S.app=q.get('app'); // honour permalinks before first paint
    applyURL(q);
    const s=await checkSession();
    if(s.ok){
      S.sessionExpires=s.expiresAt;
      S.user=s.user;S.role=s.role;S.authEnabled=s.authEnabled;
      $('#adminTab').style.display=S.role==='admin'?'':'none';
      if(s.authEnabled){
        $('#logoutBtn').style.display='';
        const initials=(S.user||'?').trim();
        $('#logoutBtn').textContent=(initials.charAt(0)||'?').toUpperCase();
        $('#logoutBtn').title='Log out ('+S.user+')';
      }
      hideLogin();
      await loadCrashCounts();
      await loadHosts();
      refreshApps().then(()=>crashChip());
      setInterval(refreshApps,10000);setInterval(crashChip,60000);
      if(s.authEnabled)setInterval(sessionWarning,60000);
      if(s.authEnabled)sessionWarning();
      show(); // dashboard is the default landing view; logs tab needs no restore
    }else{
      showLogin();
    }
  })();
  addEventListener('hashchange',()=>{if(S.tab==='crashes')loadCrashes();});
  document.addEventListener('keydown',e=>{
    if(e.key==='/'&&!e.target.matches('input,textarea,select,button,[contenteditable]')){
      e.preventDefault();$('#q').focus();
    }
  });
});
