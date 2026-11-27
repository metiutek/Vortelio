// ── Firebase config — sostituisci con i tuoi valori ──────────
const firebaseConfig = {
  apiKey:            "YOUR_API_KEY",
  authDomain:        "vortelio-3e7a8.firebaseapp.com",
  projectId:         "vortelio-3e7a8",
  storageBucket:     "vortelio-3e7a8.appspot.com",
  messagingSenderId: "YOUR_SENDER_ID",
  appId:             "YOUR_APP_ID",
};

// ── Firebase init ─────────────────────────────────────────────
let _fbUser = null;

function initFirebase() {
  if (typeof firebase === 'undefined') return;
  if (!firebase.apps.length) firebase.initializeApp(firebaseConfig);
  firebase.auth().onAuthStateChanged(user => {
    _fbUser = user;
    updateAuthUI();
    if (user) checkPendingPlan();
  });
}

function updateAuthUI() {
  const loginBtn   = document.getElementById('nav-login');
  const accountEl  = document.getElementById('nav-account');
  if (!loginBtn) return;
  if (_fbUser) {
    loginBtn.style.display  = 'none';
    if (accountEl) { accountEl.textContent = _fbUser.email || 'Account'; accountEl.style.display = ''; }
  } else {
    loginBtn.style.display  = '';
    if (accountEl) accountEl.style.display = 'none';
  }
}

// ── Stripe checkout ───────────────────────────────────────────
async function upgrade(plan) {
  if (!_fbUser) {
    sessionStorage.setItem('pendingPlan', plan);
    sessionStorage.setItem('loginRedirect', location.href);
    window.location.href = 'login.html';
    return;
  }

  const btns = document.querySelectorAll(`[data-plan="${plan}"] .upgrade-btn`);
  btns.forEach(b => { b.textContent = 'Loading…'; b.disabled = true; });

  try {
    const token  = await _fbUser.getIdToken();
    const origin = location.origin;
    const res = await fetch('/api/checkout', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Authorization': 'Bearer ' + token },
      body: JSON.stringify({
        plan,
        email:       _fbUser.email,
        uid:         _fbUser.uid,
        success_url: origin + '/success.html?plan=' + plan,
        cancel_url:  origin + '/index.html#pricing',
      }),
    });
    const data = await res.json();
    if (data.url) {
      window.location.href = data.url;
    } else {
      toast('❌ ' + (data.error || 'Checkout error'), true);
      btns.forEach(b => { b.textContent = 'Upgrade →'; b.disabled = false; });
    }
  } catch (e) {
    toast('❌ ' + e.message, true);
    btns.forEach(b => { b.textContent = 'Upgrade →'; b.disabled = false; });
  }
}

// ── Download ──────────────────────────────────────────────────
function downloadLatest(os) {
  const base = 'https://github.com/metiutek/Vortelio/releases/latest/download/';
  const map  = {
    windows: 'vortelio-windows-amd64.exe',
    mac:     'vortelio-darwin-amd64',
    linux:   'vortelio-linux-amd64',
  };
  if (map[os]) window.location.href = base + map[os];
}

// ── Pending plan after login ──────────────────────────────────
function checkPendingPlan() {
  const plan = sessionStorage.getItem('pendingPlan');
  if (plan && _fbUser) {
    sessionStorage.removeItem('pendingPlan');
    upgrade(plan);
  }
}

// ── Toast ─────────────────────────────────────────────────────
function toast(msg, isErr) {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.style.borderColor = isErr ? '#f87171' : '';
  el.classList.add('show');
  setTimeout(() => el.classList.remove('show'), 3500);
}

// ── Boot ──────────────────────────────────────────────────────
document.addEventListener('DOMContentLoaded', initFirebase);
