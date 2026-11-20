// Generates docs/charts/token-savings.svg (render to PNG with --png, see below).
//
// Data: "HeurChain on LoCoMo", section 2 (the memory grid) and the same-window
// table, https://heurchain.com/research/papers/heurchain-on-locomo/
// LoCoMo DEV, 543 scored questions; same readers, judge (gpt-6-luna, pinned
// rubric) and harness for every arm. "Whole conversation" is the arm with no
// memory system that sends the entire dialogue history (the paper gives about
// 117K input tokens per question). HeurChain is the p04a retrieval
// configuration: ranked whole units packed into a 16 KB evidence budget.
// With no memory at all, every reader answers 0 of 543.
//
//   node docs/charts/token-savings.mjs            # writes the SVG
//   node docs/charts/token-savings.mjs --png      # also renders PNG with headless Chrome/Edge
import {writeFileSync,existsSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {fileURLToPath,pathToFileURL} from 'node:url';
import {dirname,join} from 'node:path';

const TOTAL=543,FULL_TOKENS=117000;
const readers=[
  {name:'gpt-6-luna',full:426,hc:436.3,hcTokens:4932},
  {name:'Muse Spark 1.3',full:495,hc:469,hcTokens:4887},
  {name:'DeepSeek V4.1 Flash',full:492,hc:467.5,hcTokens:5028},
];
const C={surface:'#fcfcfb',card:'#ffffff',border:'#e3e2dd',grid:'#ebeae6',axis:'#c9c8c2',text:'#0b0b0b',text2:'#52514e',muted:'#75746f',full:'#eb6834',hc:'#2a78d6'};
const W=1200,H=560,PAD=40;
const fmt=n=>n.toLocaleString('en-US',{maximumFractionDigits:1});
const pct=(a,b)=>(100*a/b).toFixed(1)+'%';
const esc=s=>String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;');
const font='font-family="Segoe UI, -apple-system, Helvetica, Arial, sans-serif"';
const t=(x,y,s,{size=13,fill=C.text,weight=400,anchor='start'}={})=>`<text x="${x}" y="${y}" ${font} font-size="${size}" font-weight="${weight}" fill="${fill}" text-anchor="${anchor}">${esc(s)}</text>`;
// Horizontal bar with a 4px rounded data end and a square baseline end.
const bar=(x,y,w,h,fill)=>{const r=Math.min(4,w/2);return `<path d="M${x},${y} H${x+w-r} Q${x+w},${y} ${x+w},${y+r} V${y+h-r} Q${x+w},${y+h} ${x+w-r},${y+h} H${x} Z" fill="${fill}"/>`;};

const labelW=170,top=150,group=96,barH=22,gap=2;
const L={x:PAD+labelW,w:400},R={x:PAD+labelW+400+70,w:H};R.w=W-PAD-R.x-90;
const out=[];
out.push(`<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-labelledby="t d">`);
out.push(`<title id="t">HeurChain keeps 95 to 102 percent of whole-conversation accuracy on about 4% of the input tokens</title>`);
out.push(`<desc id="d">${esc(readers.map(r=>`${r.name}: whole conversation ${r.full} of ${TOTAL} correct on about 117,000 tokens per question; HeurChain ${r.hc} correct on ${r.hcTokens} tokens. No memory: 0 correct.`).join(' '))}</desc>`);
out.push(`<rect x="0.5" y="0.5" width="${W-1}" height="${H-1}" rx="12" fill="${C.card}" stroke="${C.border}"/>`);
out.push(t(PAD,48,'HeurChain keeps 95–102% of whole-conversation accuracy on about 4% of the tokens',{size:22,weight:700}));
out.push(t(PAD,74,`LoCoMo DEV, ${TOTAL} scored questions. Same readers, judge, rubric and harness for both arms; no compression, only retrieval.`,{size:13.5,fill:C.text2}));
out.push(t(PAD,94,`Without a memory system an agent either knows nothing (0 of ${TOTAL} correct for every reader) or carries the whole history into every request.`,{size:13.5,fill:C.text2}));

// Panel headers
out.push(t(L.x,top-22,'Correct answers',{size:14,weight:700}));
out.push(t(R.x,top-22,'Input tokens per question',{size:14,weight:700}));
const plotH=group*readers.length;
// Gridlines and axes
const lt=[0,100,200,300,400,500],rt=[0,20000,40000,60000,80000,100000,120000];
const lx=v=>L.x+L.w*v/TOTAL,rx=v=>R.x+R.w*v/120000;
for(const v of lt){out.push(`<line x1="${lx(v)}" x2="${lx(v)}" y1="${top-6}" y2="${top+plotH}" stroke="${C.grid}"/>`);out.push(t(lx(v),top+plotH+18,String(v),{size:11.5,fill:C.muted,anchor:'middle'}));}
for(const v of rt){out.push(`<line x1="${rx(v)}" x2="${rx(v)}" y1="${top-6}" y2="${top+plotH}" stroke="${C.grid}"/>`);out.push(t(rx(v),top+plotH+18,v?(v/1000)+'K':'0',{size:11.5,fill:C.muted,anchor:'middle'}));}
out.push(t(L.x,top+plotH+38,`Correct of ${TOTAL} (longer is better)`,{size:12,fill:C.text2}));
out.push(t(R.x,top+plotH+38,'Mean input tokens per question (shorter is better)',{size:12,fill:C.text2}));

readers.forEach((r,i)=>{
  const y0=top+i*group+14;
  out.push(t(PAD,y0+barH+gap/2+5,r.name,{size:13.5,weight:600}));
  // Left: accuracy
  out.push(bar(L.x,y0,lx(r.full)-L.x,barH,C.full));
  out.push(t(lx(r.full)+6,y0+16,`${fmt(r.full)}  (${pct(r.full,TOTAL)})`,{size:12.5,fill:C.text2}));
  out.push(bar(L.x,y0+barH+gap,lx(r.hc)-L.x,barH,C.hc));
  out.push(t(lx(r.hc)+6,y0+barH+gap+16,`${fmt(r.hc)}  (${pct(r.hc,TOTAL)})`,{size:12.5,fill:C.text}));
  // Right: tokens
  out.push(bar(R.x,y0,rx(FULL_TOKENS)-R.x,barH,C.full));
  out.push(t(rx(FULL_TOKENS)+6,y0+16,'≈117,000',{size:12.5,fill:C.text2}));
  out.push(bar(R.x,y0+barH+gap,Math.max(rx(r.hcTokens)-R.x,3),barH,C.hc));
  out.push(t(rx(r.hcTokens)+6,y0+barH+gap+16,`${fmt(r.hcTokens)}  (${pct(r.hcTokens,FULL_TOKENS)} of the whole conversation)`,{size:12.5,fill:C.text,weight:600}));
});
out.push(`<line x1="${L.x}" x2="${L.x}" y1="${top-6}" y2="${top+plotH}" stroke="${C.axis}"/>`);
out.push(`<line x1="${R.x}" x2="${R.x}" y1="${top-6}" y2="${top+plotH}" stroke="${C.axis}"/>`);

// Legend
const ly=top+plotH+72;let lxp=PAD;
for(const [color,label] of [[C.full,'Whole conversation: no memory system, the harness sends the entire history'],[C.hc,'HeurChain: retrieval into a 16 KB evidence budget (configuration p04a)']]){
  out.push(`<rect x="${lxp}" y="${ly-11}" width="14" height="14" rx="3" fill="${color}"/>`);
  out.push(t(lxp+20,ly,label,{size:12.5,fill:C.text2}));lxp+=label.length*6.6+60;
}
out.push(t(PAD,H-22,'Source: HeurChain on LoCoMo, heurchain.com/research/papers/heurchain-on-locomo. Whole-conversation tokens are the paper’s approximate figure.',{size:11.5,fill:C.muted}));
out.push('</svg>');

const here=dirname(fileURLToPath(import.meta.url)),svgFile=join(here,'token-savings.svg');
writeFileSync(svgFile,out.join('\n')+'\n');
console.log('wrote',svgFile);
if(process.argv.includes('--png')){
  const browser=[process.env.CHROME,'C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','/usr/bin/google-chrome','/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].filter(Boolean).find(existsSync);
  if(!browser)throw Error('No Chrome or Edge found; set CHROME.');
  const png=join(here,'token-savings.png');
  const r=spawnSync(browser,['--headless=new','--disable-gpu','--hide-scrollbars','--force-device-scale-factor=2',`--window-size=${W},${H}`,`--screenshot=${png}`,'--default-background-color=00000000',pathToFileURL(svgFile).href],{encoding:'utf8',timeout:60000});
  if(r.status!==0&&!existsSync(png))throw Error('PNG render failed: '+r.stderr);
  console.log('wrote',png);
}
