// The page's language, picked at the top right: System is Chinese when the
// browser's language is, else English. The pages are written in English;
// in Chinese every text and placeholder that is a UI string is translated
// as it reaches the page, and put back when English is picked again. The
// pick is kept in a cookie, which every fibhub page on the same host shares
// whatever its port.
(()=>{
const zh={
 // pages and sections
 'fibhub server':'fibhub 服务器','fibhub pod':'fibhub Pod','fibhub client':'fibhub 客户端',
 'Servers':'服务器','Pods':'Pod','Clients':'客户端','Tunnels':'隧道','Nodes':'节点','This web page':'本页面','Pods and nodes':'Pod 与节点',
 'Forwards':'转发','Web proxies':'Web 代理','Client info':'客户端信息','Nodes this pod can reach':'本 Pod 可访问的节点','Login':'登录',
 // table heads
 'Name':'名称','Address':'地址','Address (host:port)':'地址 (host:port)','Description':'描述','User':'用户名','Password':'密码',
 'Host':'主机','Since':'已连接','Info':'信息','Client':'客户端','Pod':'Pod','Node':'节点','Up':'上行','Down':'下行','Age':'时长',
 'State':'状态','Server':'服务器','Web page':'Web 页面','Listen':'监听','Web user':'Web 用户名','Web password':'Web 密码',
 'Server page over WebSocket':'经 WebSocket 的服务器页面','Local client':'本地客户端','Sent':'发送','Received':'接收','Local':'本地','In':'入','Out':'出',
 // buttons
 'Query':'查询','Edit nodes':'编辑节点','Kick':'踢出','Close':'关闭','Add node':'添加节点','Cancel':'取消','Save to pod':'保存到 Pod',
 'Connect':'连接','Forward':'转发','Forward…':'转发…','Update servers':'更新到服务器','Save':'保存','Open':'打开','Show':'显示','Hide':'隐藏',
 'Set login':'设置登录','Query pods':'查询 Pod','Remove':'移除','Revert':'还原','Save and tell servers':'保存并通知服务器',
 // states and notes
 'None':'无','none':'无','no login':'无登录','no web proxy':'无 Web 代理','web proxy only':'仅 Web 代理','Management page':'管理页面','Via':'经由','bodies encrypted':'消息体已加密','listening':'监听中','any':'任意',
 'registered':'已注册','connecting':'连接中','failed':'失败','stopped':'已停止',
 'Empty user and password: the page has no login.':'用户名和密码都留空：该页面不需要登录。',
 // the theme and language pickers
 'Theme':'主题','Language':'语言','System':'跟随系统','Light':'浅色','Dark':'深色','Ghibli':'吉卜力','Mecha':'机甲'};
// Placeholders, some of which are too plain a word to translate as text.
const zhPh={'name (optional)':'名称（可选）','host:7000 or ws://host:7001/fibhub':'host:7000 或 ws://host:7001/fibhub','password':'密码',
 'body AES password (optional)':'消息体 AES 密码（可选）','local address, 127.0.0.1:2222':'本地地址，如 127.0.0.1:2222','pod':'Pod','node':'节点',
 'server (optional: any)':'服务器（可选，默认任意）','web page user:password (optional)':'Web 页面的 用户名:密码（可选）',
 'key=value, key2=value2 — told to servers':'key=value, key2=value2 —— 告知服务器','user':'用户名','name':'名称','optional':'可选'};
Object.assign(zh,{
 // messages
 'Pod queried':'已查询 Pod','Pods queried':'已查询 Pod','Kicked':'已踢出','Tunnel closed':'隧道已关闭','Nodes saved':'节点已保存',
 'Nodes saved on the pod':'节点已保存到 Pod','Server added':'服务器已添加','Removed':'已移除','Forward added':'转发已添加',
 // tunnel diagrams
 'Details':'详情','← All tunnels':'← 全部隧道','Tunnel map':'隧道全景','Tunnel path':'隧道链路','Tunnel':'隧道','Control link':'控制连接',
 'Traffic':'流量热度','low → high':'低 → 高','Tunnel ID':'隧道 ID','Local app':'本地应用','this page':'本页','not visible here':'此处不可见',
 'Only the client sees the local app.':'只有客户端能看到本地应用。','This tunnel is closed.':'该隧道已关闭。',
 'No open tunnels; dashed lines are control links.':'暂无隧道；虚线为控制连接。','Protocol':'协议','Body encryption':'消息体加密',
 'Version':'版本','Latency':'延迟','Share':'分担','Connect':'建连','OS':'系统','Uptime':'运行时长','arpc over WebSocket':'arpc（WebSocket）','arpc over TCP':'arpc（TCP）',
 'Web page over WebSocket':'经 WebSocket 的 Web 页面',
 'Forward removed':'转发已移除','Info sent to servers':'信息已发送到服务器','Login saved':'登录已保存','Kick this peer?':'踢出该 peer？'});
// Texts made of a UI string and names.
const zhRe=[[/^Nodes of pod (.+) \(via (.+)\)$/,'Pod $1 的节点（经由 $2）'],[/^Nodes of pod (.+)$/,'Pod $1 的节点'],
 [/^Login of the web page of server (.+)$/,'服务器 $1 的 Web 页面登录'],[/^Login of the web page of (.+)$/,'$1 的 Web 页面登录'],
 [/^Disconnect from (.+)\?$/,'断开与 $1 的连接？'],[/^via (.+)$/,'经由 $1'],[/^Tunnels \((\d+)\)$/,'隧道（$1）'],[/^(\d+) forwards$/,'$1 个转发'],[/^(\d+) nodes$/,'$1 个节点'],[/^(\d+)% loss$/,'丢包 $1%'],[/^(.+) · (\d+)% loss$/,'$1 · 丢包 $2%']];
// Parts of the header's status line.
const zhMeta=[[' · bodies encrypted',' · 消息体已加密'],[' · up ',' · 已运行 '],[' · settings not saved (no -config)',' · 设置未保存（未指定 -config）']];
// A table cell's own text, the name in the header and a .raw element's text
// are data (a name, an address), except these.
const cellOK=new Set(['any','none']);

const langs=[['auto','System'],['en','English'],['zh','中文']];
const saved=()=>{const m=document.cookie.match(/(?:^|;\s*)fibhub-lang=([a-z]+)/);return m&&langs.some(l=>l[0]===m[1])?m[1]:'auto'};
const system=()=>/^zh\b/i.test(navigator.language||'')?'zh':'en';
let pref=saved(),lang=pref==='auto'?system():pref;

// tr is s in Chinese, or null if it is no UI string; el holds it, as text,
// or as a placeholder if ph.
function tr(s,el,ph){const k=s.trim(),d=ph?zhPh:zh;if(!k)return null;
 if(Object.hasOwn(d,k)&&!(el&&(el.tagName==='TD'||el.id==='name'||el.classList.contains('raw'))&&!cellOK.has(k)))return s.replace(k,d[k]);
 for(const[re,r]of zhRe)if(re.test(k))return s.replace(k,k.replace(re,r));
 // A diagram's columns, "Clients → Server → Pods".
 const parts=k.split(' → ');if(parts.length>1&&parts.every(p=>Object.hasOwn(zh,p)))return s.replace(k,parts.map(p=>zh[p]).join(' → '));
 if(el&&el.id==='meta'){let o=s;for(const[a,b]of zhMeta)o=o.split(a).join(b);if(o!==s)return o}
 return null}
// Each translated text node and placeholder keeps its English, to be put
// back, as long as it still holds the translation.
function text(n){const el=n.parentElement;if(!el||el.closest('script,style'))return;
 const v=n.nodeValue,en=n._en!==undefined&&v===n._zh?n._en:v,t=lang==='zh'?tr(en,el):null;
 if(t!==null){n._en=en;n._zh=t;if(v!==t)n.nodeValue=t}else if(en!==v)n.nodeValue=en}
function placeholder(el){const v=el.getAttribute('placeholder');if(v===null)return;
 const en=el._en!==undefined&&v===el._zh?el._en:v,t=lang==='zh'?tr(en,null,true):null;
 if(t!==null){el._en=en;el._zh=t;if(v!==t)el.setAttribute('placeholder',t)}else if(en!==v)el.setAttribute('placeholder',en)}
function walk(root){if(root.nodeType===3)return text(root);if(root.nodeType!==1)return;
 if(root.hasAttribute('placeholder'))placeholder(root);
 const w=document.createTreeWalker(root,NodeFilter.SHOW_TEXT|NodeFilter.SHOW_ELEMENT);
 for(let n=w.nextNode();n;n=w.nextNode())n.nodeType===3?text(n):n.hasAttribute('placeholder')&&placeholder(n)}
function apply(){document.documentElement.lang=lang==='zh'?'zh-CN':'en';walk(document.documentElement);
 const s=document.getElementById('lang');if(s)s.value=pref}

new MutationObserver(ms=>{if(lang!=='zh'&&!ms.some(m=>m.target._en!==undefined))return;
 for(const m of ms){if(m.type==='childList')m.addedNodes.forEach(walk);else if(m.type==='characterData')text(m.target);else placeholder(m.target)}})
 .observe(document.documentElement,{childList:true,subtree:true,characterData:true,attributes:true,attributeFilter:['placeholder']});
const confirm0=window.confirm;window.confirm=m=>confirm0.call(window,lang==='zh'&&tr(String(m))||m);
apply();

document.addEventListener('DOMContentLoaded',()=>{apply();const h=document.querySelector('header');if(!h)return;
 const l=document.createElement('label');l.className='pref lang';
 l.innerHTML='<span>Language</span><select id="lang">'+langs.map(([v,n])=>`<option value="${v}">${n}</option>`).join('')+'</select>';
 h.appendChild(l);const s=l.querySelector('select');s.value=pref;
 s.onchange=()=>{document.cookie='fibhub-lang='+s.value+';path=/;max-age=31536000;samesite=lax';pref=s.value;lang=pref==='auto'?system():pref;apply()}});
// Follow a pick made on another page, or a change of the system's language.
setInterval(()=>{const p=saved(),l=p==='auto'?system():p;if(p!==pref||l!==lang){pref=p;lang=l;apply()}},1500);
})();
