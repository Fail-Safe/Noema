#!/bin/zsh

set -euo pipefail

if [[ $# -ne 1 ]]; then
    print -u2 "usage: ${0:t} /absolute/path/to/noema"
    exit 2
fi

destination=${1:A}
launch_agents_dir=${NOEMA_LAUNCH_AGENTS_DIR:-"$HOME/Library/LaunchAgents"}
launch_domain=${NOEMA_LAUNCHD_DOMAIN:-"gui/$(id -u)"}

if [[ ! -d "$launch_agents_dir" ]]; then
    exit 0
fi

typeset -a plists labels keepalive_policies

for plist in "$launch_agents_dir"/*.plist(N); do
    program=$(plutil -extract ProgramArguments.0 raw "$plist" 2>/dev/null || true)
    if [[ -z "$program" || "${program:A}" != "$destination" ]]; then
        continue
    fi

    if ! plutil -lint "$plist" >/dev/null; then
        print -u2 "error: matching LaunchAgent is not a valid plist: $plist"
        exit 1
    fi

    label=$(plutil -extract Label raw "$plist" 2>/dev/null || true)
    if [[ -z "$label" ]]; then
        print -u2 "error: matching LaunchAgent has no Label: $plist"
        exit 1
    fi

    keepalive=$(plutil -extract KeepAlive json -o - "$plist" 2>/dev/null || true)
    plists+=("$plist")
    labels+=("$label")
    keepalive_policies+=("$keepalive")
done

if (( ${#plists} == 0 )); then
    exit 0
fi

for index in {1..${#plists}}; do
    plist=${plists[$index]}
    label=${labels[$index]}
    keepalive=${keepalive_policies[$index]}

    typeset -a arguments localhost_value_indices
    argument_index=0
    while argument=$(plutil -extract "ProgramArguments.$argument_index" raw "$plist" 2>/dev/null); do
        arguments+=("$argument")
        (( argument_index += 1 ))
    done

    has_ipv4_loopback=false
    has_ipv6_loopback=false
    for (( argument_index = 1; argument_index < ${#arguments}; argument_index++ )); do
        if [[ ${arguments[$argument_index]} != "--host" ]]; then
            continue
        fi
        value=${arguments[$(( argument_index + 1 ))]}
        case "$value" in
            127.0.0.1) has_ipv4_loopback=true ;;
            ::1) has_ipv6_loopback=true ;;
            localhost) localhost_value_indices+=("$argument_index") ;;
        esac
    done

    if (( ${#localhost_value_indices} > 0 )); then
        backup="${plist}.pre-noema-loopback"
        if [[ ! -e "$backup" ]]; then
            cp -p "$plist" "$backup"
        fi
        for (( argument_index = ${#localhost_value_indices}; argument_index >= 1; argument_index-- )); do
            value_index=${localhost_value_indices[$argument_index]}
            flag_index=$(( value_index - 1 ))
            if [[ "$has_ipv4_loopback" == true && "$has_ipv6_loopback" == true ]]; then
                plutil -remove "ProgramArguments.$value_index" "$plist"
                plutil -remove "ProgramArguments.$flag_index" "$plist"
            elif [[ "$has_ipv4_loopback" == true ]]; then
                plutil -remove "ProgramArguments.$value_index" "$plist"
                plutil -insert "ProgramArguments.$value_index" -string "::1" "$plist"
                has_ipv6_loopback=true
            elif [[ "$has_ipv6_loopback" == true ]]; then
                plutil -remove "ProgramArguments.$value_index" "$plist"
                plutil -insert "ProgramArguments.$value_index" -string "127.0.0.1" "$plist"
                has_ipv4_loopback=true
            else
                plutil -remove "ProgramArguments.$value_index" "$plist"
                plutil -insert "ProgramArguments.$value_index" -string "127.0.0.1" "$plist"
                plutil -insert "ProgramArguments.$(( value_index + 1 ))" -string "--host" "$plist"
                plutil -insert "ProgramArguments.$(( value_index + 2 ))" -string "::1" "$plist"
                has_ipv4_loopback=true
                has_ipv6_loopback=true
            fi
        done
        plutil -lint "$plist" >/dev/null
        print "Normalized launchd loopback listeners: $plist"
    fi

    if [[ "$keepalive" == '{"SuccessfulExit":false}' ]]; then
        backup="${plist}.pre-noema-keepalive"
        if [[ ! -e "$backup" ]]; then
            cp -p "$plist" "$backup"
        fi
        plutil -replace KeepAlive -bool true "$plist"
        plutil -lint "$plist" >/dev/null
        print "Updated launchd keepalive policy: $plist"
    elif [[ "$keepalive" == *'"SuccessfulExit":false'* ]]; then
        print -u2 "warning: preserving custom KeepAlive policy: $plist"
    fi

    if launchctl print "$launch_domain/$label" >/dev/null 2>&1; then
        launchctl bootout "$launch_domain/$label"
        reloaded=false
        for attempt in 1 2 3; do
            if launchctl bootstrap "$launch_domain" "$plist"; then
                reloaded=true
                break
            fi
            if (( attempt < 3 )); then
                print -u2 "warning: launchd bootstrap attempt $attempt failed for $label; retrying"
                sleep 1
            fi
        done
        if [[ "$reloaded" != true ]]; then
            print -u2 "error: launchd bootstrap failed after 3 attempts: $label"
            exit 1
        fi
        print "Reloaded launchd agent: $label"
    fi
done
