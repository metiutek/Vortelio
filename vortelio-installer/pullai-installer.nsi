; ============================================================
;  Vortelio Installer — NSIS Script v0.1.0
; ============================================================

Unicode True
SetCompressor /SOLID lzma

!include "MUI2.nsh"
!include "LogicLib.nsh"
!include "WinVer.nsh"
!include "x64.nsh"
!include "StrFunc.nsh"
${StrStr}

!define PRODUCT_NAME       "Vortelio"
!define PRODUCT_VERSION    "0.4.6"
!define PRODUCT_PUBLISHER  "Vortelio Project"
!define PRODUCT_URL        "https://github.com/vortelio/vortelio"
!define UNINST_KEY  "Software\Microsoft\Windows\CurrentVersion\Uninstall\Vortelio"
!define APP_PATHS   "Software\Microsoft\Windows\CurrentVersion\App Paths\vortelio.exe"

!define MUI_ABORTWARNING
!define MUI_ICON "assets\vortelio.ico"
!define MUI_UNICON "assets\vortelio.ico"

!define MUI_WELCOMEPAGE_TITLE "Benvenuto in Vortelio ${PRODUCT_VERSION}"
!define MUI_WELCOMEPAGE_TEXT "Vortelio ti permette di eseguire modelli AI in locale:$\r$\n  • LLM — Chat, Mistral, Llama3, Phi3$\r$\n  • Immagini — Stable Diffusion XL, FLUX$\r$\n  • Audio — Whisper e Bark$\r$\n  • Video — CogVideoX, AnimateDiff$\r$\n$\r$\nFai clic su Avanti per continuare."

!define MUI_FINISHPAGE_TITLE "Vortelio installato!"
!define MUI_FINISHPAGE_TEXT "Vortelio ${PRODUCT_VERSION} è pronto.$\r$\n$\r$\nPuoi iniziare subito:$\r$\n  • Spunta la casella qui sotto per aprire la GUI$\r$\n  • Oppure cerca 'Vortelio' nella barra di Windows$\r$\n$\r$\nComandi rapidi:$\r$\n  vortelio pull llm/mistral:7b$\r$\n  vortelio run llm/mistral:7b"
!define MUI_FINISHPAGE_RUN       "$INSTDIR\vortelio.exe"
!define MUI_FINISHPAGE_RUN_PARAMETERS "gui"
!define MUI_FINISHPAGE_RUN_TEXT  "Apri la Web UI di Vortelio"
!define MUI_FINISHPAGE_LINK      "Documentazione su GitHub"
!define MUI_FINISHPAGE_LINK_LOCATION "${PRODUCT_URL}"

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "LICENSE.txt"
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "Italian"
!insertmacro MUI_LANGUAGE "English"

Name          "${PRODUCT_NAME} ${PRODUCT_VERSION}"
OutFile       "Vortelio-Setup-${PRODUCT_VERSION}.exe"
InstallDir    "$PROGRAMFILES64\Vortelio"
InstallDirRegKey HKLM "${APP_PATHS}" ""
ShowInstDetails   show
ShowUnInstDetails show
RequestExecutionLevel admin

VIProductVersion "${PRODUCT_VERSION}.0"
VIAddVersionKey /LANG=0 "ProductName"     "${PRODUCT_NAME}"
VIAddVersionKey /LANG=0 "ProductVersion"  "${PRODUCT_VERSION}"
VIAddVersionKey /LANG=0 "CompanyName"     "Metiu"
VIAddVersionKey /LANG=0 "FileDescription" "Vortelio — Run AI models locally"
VIAddVersionKey /LANG=0 "FileVersion"     "${PRODUCT_VERSION}.0"
VIAddVersionKey /LANG=0 "LegalCopyright"  "Copyright 2025 Metiu — Apache-2.0"
VIAddVersionKey /LANG=0 "OriginalFilename" "Vortelio-Setup.exe"
VIAddVersionKey /LANG=0 "Comments"        "Run AI models locally — by Metiu"
VIAddVersionKey /LANG=0 "InternalName"    "Vortelio"
VIAddVersionKey /LANG=0 "Publisher"       "Metiu"

Function .onInit
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_ICONSTOP "Vortelio richiede Windows 10 o successivo."
    Abort
  ${EndIf}
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "Vortelio richiede Windows a 64-bit."
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

; ════════════════════════════════════════════════════════════
;  SECTION 1: Core (obbligatorio)
; ════════════════════════════════════════════════════════════
Section "Vortelio CLI + Web UI (obbligatorio)" SecCore
  SectionIn RO

  ; Termina eventuali istanze in esecuzione prima di sovrascrivere
  DetailPrint "Chiusura istanze Vortelio..."
  nsExec::ExecToLog 'taskkill /F /IM vortelio-server.exe /T'
  nsExec::ExecToLog 'taskkill /F /IM vortelio.exe /T'
  Sleep 1200

  SetOutPath "$INSTDIR"
  SetOverwrite on

  ; Ferma vortelio.exe se in esecuzione prima di sovrascriverlo
  ; (evita "Errore nell'apertura del file per la scrittura")
  DetailPrint "Arresto eventuali istanze di vortelio in esecuzione..."
  nsExec::ExecToLog 'taskkill /F /IM vortelio.exe /T'
  Sleep 500

  ; Ferma vortelio se è già in esecuzione (altrimenti il file è bloccato)
  DetailPrint "Chiusura processi Vortelio in corso..."
  nsExec::ExecToLog 'taskkill /F /IM vortelio.exe /T'
  Sleep 1000

  DetailPrint "Installazione vortelio.exe..."
  ; Copia e rinomina il binario a vortelio.exe
  File "/oname=vortelio.exe" "build\pullai.exe"
  File "/oname=vortelio-server.exe" "build\pullai-server.exe"

  DetailPrint "Installazione script di supporto..."
  File "build\install-llama.ps1"

  ; ── Launcher VBS: usa vortelio.exe gui (avvia server detached + apre Edge) ──
  DetailPrint "Creazione launcher desktop..."
  FileOpen $0 "$INSTDIR\vortelio-start.vbs" w
  FileWrite $0 "Set o = WScript.CreateObject($\"WScript.Shell$\")$\r$\n"
  FileWrite $0 "o.Run $\"$INSTDIR\vortelio.exe gui$\"$\r$\n"
  FileClose $0
  ; ── BAT per terminale (per chi preferisce CLI) ────────────────────────────
  FileOpen $0 "$INSTDIR\vortelio-terminal.bat" w
  FileWrite $0 "@echo off$\r$\n"
  FileWrite $0 "title Vortelio CLI$\r$\n"
  FileWrite $0 "cd /d $\"%USERPROFILE%$\"$\r$\n"
  FileWrite $0 "$\"$INSTDIR\vortelio.exe$\" help$\r$\n"
  FileWrite $0 "cmd /k$\r$\n"
  FileClose $0

  ; Registro — vortelio.exe per barra di ricerca Windows
  WriteRegStr HKLM "${APP_PATHS}" "" "$INSTDIR\vortelio.exe"
  WriteRegStr HKLM "${APP_PATHS}" "Path" "$INSTDIR"

  ; Registra "vortelio-gui" per aprire la GUI dalla barra di ricerca Win+R / barra di ricerca
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\vortelio-gui.exe" "" "$INSTDIR\vortelio.exe"
  WriteRegStr HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\vortelio-gui.exe" "Path" "$INSTDIR"

  ; AppUserModelID per taskbar Windows 10/11
  WriteRegStr HKCU "Software\Classes\AppUserModelId\Vortelio.App" "DisplayName" "Vortelio"
  WriteRegStr HKCU "Software\Classes\AppUserModelId\Vortelio.App" "IconUri" "$INSTDIR\vortelio.exe"
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayName"          "${PRODUCT_NAME} ${PRODUCT_VERSION}"
  WriteRegStr   HKLM "${UNINST_KEY}" "UninstallString"      '"$INSTDIR\Uninstall.exe"'
  WriteRegStr   HKLM "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayIcon"          "$INSTDIR\vortelio.exe,0"
  WriteRegStr   HKLM "${UNINST_KEY}" "DisplayVersion"       "${PRODUCT_VERSION}"
  WriteRegStr   HKLM "${UNINST_KEY}" "Publisher"            "${PRODUCT_PUBLISHER}"
  WriteRegStr   HKLM "${UNINST_KEY}" "URLInfoAbout"         "${PRODUCT_URL}"
  WriteRegStr   HKLM "${UNINST_KEY}" "InstallLocation"      "$INSTDIR"
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoModify"     1
  WriteRegDWORD HKLM "${UNINST_KEY}" "NoRepair"     1
  WriteRegDWORD HKLM "${UNINST_KEY}" "EstimatedSize" 10240
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  ; Start Menu
  CreateDirectory "$SMPROGRAMS\Vortelio"
  CreateShortcut "$SMPROGRAMS\Vortelio\Vortelio.lnk"      "$INSTDIR\vortelio.exe" "gui" "$INSTDIR\vortelio.exe" 0

  CreateShortcut "$SMPROGRAMS\Vortelio\Vortelio Terminale.lnk"   "$INSTDIR\vortelio-terminal.bat" "" "$INSTDIR\vortelio.exe" 0
  CreateShortcut "$SMPROGRAMS\Vortelio\Disinstalla Vortelio.lnk" "$INSTDIR\Uninstall.exe"

SectionEnd

; ════════════════════════════════════════════════════════════
;  SECTION 2: Desktop
; ════════════════════════════════════════════════════════════
Section "Collegamento sul Desktop" SecDesktop
  CreateShortcut "$DESKTOP\Vortelio.lnk" "$INSTDIR\vortelio.exe" "gui" "$INSTDIR\vortelio.exe" 0
SectionEnd

; ════════════════════════════════════════════════════════════
;  SECTION 3: PATH
; ════════════════════════════════════════════════════════════
Section "Aggiungi 'vortelio' al PATH di sistema" SecPath
  DetailPrint "Aggiunta di vortelio al PATH di sistema..."
  ReadRegStr $0 HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "Path"
  ${StrStr} $1 $0 "$INSTDIR"
  ${If} $1 == ""
    WriteRegExpandStr HKLM "SYSTEM\CurrentControlSet\Control\Session Manager\Environment" "Path" "$0;$INSTDIR"
    SendMessage ${HWND_BROADCAST} ${WM_WININICHANGE} 0 "STR:Environment" /TIMEOUT=5000
    DetailPrint "PATH aggiornato."
  ${Else}
    DetailPrint "PATH già configurato."
  ${EndIf}
SectionEnd

; ════════════════════════════════════════════════════════════
;  SECTION 4: Setup automatico (llama.cpp)
; ════════════════════════════════════════════════════════════
Section "Scarica llama.cpp e configura Python" SecSetup
  DetailPrint "Avvio setup automatico (silent)..."

  ; VBScript che esegue vortelio setup completamente in background (nessuna finestra CMD)
  FileOpen $0 "$INSTDIR\run-setup.vbs" w
  FileWrite $0 "Set sh = CreateObject($\"WScript.Shell$\")$\r$\n"
  FileWrite $0 "sh.Run Chr(34) & $\"$INSTDIR\vortelio.exe$\" & Chr(34) & $\" setup$\", 0, False$\r$\n"
  FileClose $0

  ; Lancia il VBS senza finestre (0 = hidden)
  nsExec::ExecToLog '"$SYSDIR\wscript.exe" //nologo "$INSTDIR\run-setup.vbs"'
  DetailPrint "Setup completato in background."
SectionEnd

; ════════════════════════════════════════════════════════════
;  Descriptions
; ════════════════════════════════════════════════════════════
!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
  !insertmacro MUI_DESCRIPTION_TEXT ${SecCore}    "Il binario vortelio.exe e la Web UI. Obbligatorio."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} "Crea un collegamento Vortelio sul Desktop."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecPath}    "Aggiunge vortelio al PATH di sistema."
  !insertmacro MUI_DESCRIPTION_TEXT ${SecSetup}   "Scarica llama.cpp e configura Python automaticamente."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

; ════════════════════════════════════════════════════════════
;  UNINSTALLER
; ════════════════════════════════════════════════════════════
Section "Uninstall"
  ; 1. Ferma il server se in esecuzione
  nsExec::ExecToLog 'taskkill /F /IM vortelio.exe /T'

  ; 2. Elimina tutti i file installati
  Delete "$INSTDIR\vortelio.exe"
  Delete "$INSTDIR\vortelio-server.exe"
  DeleteRegKey HKLM "Software\Microsoft\Windows\CurrentVersion\App Paths\vortelio-gui.exe"
  DeleteRegKey HKCU "Software\Classes\AppUserModelId\Vortelio.App"
  Delete "$INSTDIR\vortelio-start.vbs"
  Delete "$INSTDIR\vortelio-start.vbs"
  Delete "$INSTDIR\vortelio-start.bat"
  Delete "$INSTDIR\vortelio-terminal.bat"
  Delete "$INSTDIR\vortelio-ui-launcher.bat"
  Delete "$INSTDIR\install-llama.ps1"
  Delete "$INSTDIR\run-setup.bat"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir /r "$INSTDIR\bin"
  RMDir "$INSTDIR"

  ; 3. Elimina shortcuts
  Delete "$SMPROGRAMS\Vortelio\Vortelio Web UI.lnk"
  Delete "$SMPROGRAMS\Vortelio\Vortelio Terminale.lnk"
  Delete "$SMPROGRAMS\Vortelio\Disinstalla Vortelio.lnk"
  RMDir  "$SMPROGRAMS\Vortelio"
  Delete "$DESKTOP\Vortelio.lnk"

  ; 4. Rimuovi dal PATH (silenzioso, nessuna finestra)
  nsExec::ExecToLog '$WINDIR\System32\WindowsPowerShell\v1.0\powershell.exe -NonInteractive -WindowStyle Hidden -Command \
    "[Environment]::SetEnvironmentVariable($\"Path$\", (([Environment]::GetEnvironmentVariable($\"Path$\",$\"Machine$\") -split $\";$\") | Where-Object {$_ -ne $\"$INSTDIR$\" -and $_ -ne $\"$INSTDIR\bin$\"} | Where-Object {$_ -ne $\"$\"}) -join $\";\"), $\"Machine$\")"'

  ; 5. Elimina chiavi registro
  DeleteRegKey HKLM "${UNINST_KEY}"
  DeleteRegKey HKLM "${APP_PATHS}"

  ; 6. Messaggio finale — DOPO che tutto è stato eliminato
  ;    Usa $PROFILE che NSIS risolve correttamente (non %USERPROFILE% letterale)
  MessageBox MB_ICONINFORMATION "Vortelio disinstallato correttamente.$\r$\n$\r$\nI tuoi modelli sono stati conservati in:$\r$\n$PROFILE\.vortelio\$\r$\n$\r$\nPuoi eliminarli manualmente se non ti servono più."

SectionEnd
