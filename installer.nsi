; Stroom NSIS installer - per-user install, no admin needed.
; Usage: makensis -DSTROOM_VERSION=x.y.z -DSTROOM_EXE=stroom-windows-amd64.exe installer.nsi
Unicode true
SetCompressor /SOLID lzma

!ifndef STROOM_VERSION
  !define STROOM_VERSION "0.1.0"
!endif
!ifndef STROOM_EXE
  !define STROOM_EXE "stroom-windows-amd64.exe"
!endif

Name "Stroom ${STROOM_VERSION}"
OutFile "stroom-${STROOM_VERSION}-setup.exe"
InstallDir "$LOCALAPPDATA\Programs\Stroom"
InstallDirRegKey HKCU "Software\Stroom" "InstallDir"
RequestExecutionLevel user

!include "MUI2.nsh"
!define MUI_ABORTWARNING
!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Section "Stroom" SEC01
  SetOutPath "$INSTDIR"
  File "${STROOM_EXE}"
  WriteRegStr HKCU "Software\Stroom" "InstallDir" "$INSTDIR"
  WriteRegStr HKCU "Software\Stroom" "Version" "${STROOM_VERSION}"
  WriteUninstaller "$INSTDIR\uninstall.exe"

  CreateDirectory "$SMPROGRAMS\Stroom"
  CreateShortcut "$SMPROGRAMS\Stroom\Stroom.lnk" "$INSTDIR\${STROOM_EXE}" "" "$INSTDIR\${STROOM_EXE}" 0
  CreateShortcut "$SMPROGRAMS\Stroom\Uninstall Stroom.lnk" "$INSTDIR\uninstall.exe"
  CreateShortcut "$DESKTOP\Stroom.lnk" "$INSTDIR\${STROOM_EXE}" "" "$INSTDIR\${STROOM_EXE}" 0
SectionEnd

Section "Uninstall"
  Delete "$INSTDIR\${STROOM_EXE}"
  Delete "$INSTDIR\uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\Stroom\Stroom.lnk"
  Delete "$SMPROGRAMS\Stroom\Uninstall Stroom.lnk"
  RMDir "$SMPROGRAMS\Stroom"
  Delete "$DESKTOP\Stroom.lnk"
  DeleteRegKey HKCU "Software\Stroom"
SectionEnd

Function .onInstSuccess
  MessageBox MB_ICONINFORMATION "Stroom ${STROOM_VERSION} installed.\nLaunch it from the Start Menu or desktop shortcut."
FunctionEnd

VIProductVersion "${STROOM_VERSION}.0"
VIAddVersionKey "ProductName" "Stroom"
VIAddVersionKey "CompanyName" "Xodo Technology"
VIAddVersionKey "FileDescription" "Stroom BitTorrent/NZB client"
VIAddVersionKey "LegalCopyright" "Copyright (c) 2026 Xodo Technology"
VIAddVersionKey "FileVersion" "${STROOM_VERSION}"
VIAddVersionKey "ProductVersion" "${STROOM_VERSION}"
BrandingText "Stroom ${STROOM_VERSION}"
ShowInstDetails show
ShowUninstDetails show
Icon "icon.ico"
UninstallIcon "icon.ico"
